package controllers

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
)

// metadata is the context both templating layers share: the one that renders
// object names and command arguments before the plan leaves the controller,
// and the one that renders template values after everything else has resolved.
// The fields are exported because text/template can only reach exported
// fields.
type metadata struct {
	ClaimName         string
	ClaimNamespace    string
	ClaimKind         string
	ClaimAPIVersion   string
	InstanceNamespace string
	ArcanumName       string
}

// reservedNames are the metadata identifiers. A mapping key using one would
// shadow it inside template values, so they are rejected outright. Derived
// from templateContext, because that is the map a key would shadow, and a
// hand written second list would drift the moment a field is added.
//
// It returns a fresh map rather than being a package var so that callers in
// two different files cannot mutate a shared one.
func reservedNames() map[string]struct{} {
	reserved := map[string]struct{}{}
	for name := range templateContext(metadata{}, nil) {
		reserved[name] = struct{}{}
	}

	return reserved
}

// renderMetadata renders one string against the metadata context. It is the
// entry point for the fields rendered before the plan reaches the Job:
// objectRef.name and each argument of exec.command.
func renderMetadata(s string, md metadata) (string, error) {
	tmpl, err := template.New("").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, md); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return out.String(), nil
}

// templateRefs returns the top level field names a template body references,
// deduped and sorted. It walks the parse tree rather than matching the string,
// so a reference inside an if and an escaped brace both come out right.
//
// The sort is not cosmetic: validate reports the first unknown reference it
// finds, and map order would make that message change between reconciles.
func templateRefs(body string) ([]string, error) {
	tmpl, err := template.New("refs").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	seen := map[string]struct{}{}
	visit := func(fn *parse.FieldNode) {
		seen[fn.Ident[0]] = struct{}{}
	}

	// A {{define}} or {{block}} body is parsed into an associated template,
	// which tmpl.Root does not reach. A reference missed there is an edge
	// missing from the dependency graph, so walk every tree.
	for _, t := range tmpl.Templates() {
		if t.Tree == nil {
			continue
		}
		walkNode(t.Root, visit)
	}

	return slices.Sorted(maps.Keys(seen)), nil
}

// walkNode visits every field node in a parsed template. Missing a node type
// here means a reference goes unseen and the dependency graph is quietly wrong.
func walkNode(n parse.Node, visit func(*parse.FieldNode)) {
	switch v := n.(type) {
	case *parse.FieldNode:
		visit(v)
	case *parse.ListNode:
		// A nil *ListNode arrives here as a non-nil interface holding a nil
		// pointer, which is exactly what BranchNode.ElseList is when there is
		// no else. Without this guard the range below panics.
		if v == nil {
			return
		}
		for _, c := range v.Nodes {
			walkNode(c, visit)
		}
	case *parse.ActionNode:
		walkNode(v.Pipe, visit)
	case *parse.PipeNode:
		if v == nil {
			return
		}
		for _, c := range v.Cmds {
			walkNode(c, visit)
		}
	case *parse.CommandNode:
		for _, a := range v.Args {
			walkNode(a, visit)
		}
	case *parse.ChainNode:
		// A chain is (pipeline).Field. The field hanging off the end is
		// relative, but whatever the pipeline references is not.
		walkNode(v.Node, visit)
	case *parse.IfNode:
		walkBranch(&v.BranchNode, visit)
	case *parse.RangeNode:
		// range and with rebind the dot, so a field in the body belongs to
		// whatever the pipe produced and is not a top level name. Collecting
		// those would invent dependencies on keys that do not exist.
		walkNode(v.Pipe, visit)
	case *parse.WithNode:
		walkNode(v.Pipe, visit)
	case *parse.TemplateNode:
		walkNode(v.Pipe, visit)
	}
}

// walkBranch visits the pipe and both bodies of an if. range and with do
// not come through here, because their bodies see a different dot.
func walkBranch(b *parse.BranchNode, visit func(*parse.FieldNode)) {
	walkNode(b.Pipe, visit)
	walkNode(b.List, visit)
	walkNode(b.ElseList, visit)
}

// validate checks everything that can be checked without touching the
// cluster. hasClaim says whether the Arcanum's namespace carries chryso's
// claim annotation.
//
// The passes run in this order because an unknown reference would otherwise
// surface as a confusing cycle. Keys are walked sorted, not in map order, so
// an Arcanum nobody touched keeps the same status.message between reconciles.
func validate(mapping map[string]arcanav1.ValueSource, hasClaim bool) error {
	reserved := reservedNames()
	keys := slices.Sorted(maps.Keys(mapping))

	for _, k := range keys {
		if _, ok := reserved[k]; ok {
			return fmt.Errorf("key %q is a reserved metadata name", k)
		}
	}

	if !hasClaim {
		for _, k := range keys {
			if mapping[k].Source == arcanav1.SourceClaimParam {
				return fmt.Errorf("key %q uses claimParam, but this namespace has no claim", k)
			}
		}
	}

	// A path carrying its own braces would close the expression render wraps
	// it in and open another, which lets one key pull in the whole claim.
	for _, k := range keys {
		src := mapping[k]
		if src.Source == arcanav1.SourceClaimParam && strings.ContainsAny(src.Path, "{}") {
			return fmt.Errorf("key %q: claimParam path %q must not contain braces, write it bare as .spec.foo", k, src.Path)
		}
	}

	for _, k := range keys {
		src := mapping[k]
		if src.Source != arcanav1.SourceTemplate {
			continue
		}

		refs, err := templateRefs(src.Value)
		if err != nil {
			return fmt.Errorf("key %q: %w", k, err)
		}

		for _, ref := range refs {
			if _, ok := mapping[ref]; ok {
				continue
			}
			if _, ok := reserved[ref]; ok {
				continue
			}
			return fmt.Errorf("key %q references unknown name %q", k, ref)
		}
	}

	_, err := renderOrder(mapping)

	return err
}

// renderOrder returns every mapping key in an order safe to resolve in:
// non-template keys first, sorted, then template keys in dependency order.
//
// Only template entries have edges, since nothing else depends on anything.
// The tie-break is sorted because the plan hash is built from this order, so
// map iteration must not leak into it.
func renderOrder(mapping map[string]arcanav1.ValueSource) ([]string, error) {
	order := make([]string, 0, len(mapping))
	placed := map[string]struct{}{}
	pending := map[string][]string{}

	// Phase 1: place the non-template keys in sorted order, and collect each
	// template's edges for phase 2. Non-templates depend on nothing.
	for _, k := range slices.Sorted(maps.Keys(mapping)) {
		if mapping[k].Source != arcanav1.SourceTemplate {
			order = append(order, k)
			placed[k] = struct{}{}
			continue
		}

		refs, err := templateRefs(mapping[k].Value)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}

		// A ref to a metadata name is not an edge.
		var edges []string
		for _, ref := range refs {
			if _, ok := mapping[ref]; ok {
				edges = append(edges, ref)
			}
		}
		pending[k] = edges
	}

	// Phase 2: Kahn's, in sorted batches.
	unplaced := func(dep string) bool {
		_, ok := placed[dep]

		return !ok
	}

	for len(pending) > 0 {
		ready := make([]string, 0, len(pending))
		for k, edges := range pending {
			if !slices.ContainsFunc(edges, unplaced) {
				ready = append(ready, k)
			}
		}

		if len(ready) == 0 {
			left := slices.Sorted(maps.Keys(pending))
			return nil, fmt.Errorf("cycle in valueMapping: %s", strings.Join(left, ", "))
		}

		slices.Sort(ready)
		for _, k := range ready {
			order = append(order, k)
			placed[k] = struct{}{}
			delete(pending, k)
		}
	}

	return order, nil
}

// render resolves the whole mapping. gathered holds the values the Job
// produced, keyed by mapping key. claim may be nil when there is none.
//
// The returned map holds mapping keys and nothing else. Reading the referenced
// objects is the gather Job's work, so by the time this runs those values are
// already in gathered.
func render(
	mapping map[string]arcanav1.ValueSource,
	md metadata,
	gathered map[string]string,
	claim *unstructured.Unstructured,
) (map[string]string, error) {
	order, err := renderOrder(mapping)
	if err != nil {
		return nil, err
	}

	values := make(map[string]string, len(mapping))

	for _, key := range order {
		src := mapping[key]

		switch src.Source {
		case arcanav1.SourceConst:
			values[key] = src.Value

		case arcanav1.SourceObjectRef, arcanav1.SourceExec:
			v, ok := gathered[key]
			if !ok {
				return nil, fmt.Errorf(
					"internal error: the plan and valueMapping have drifted: no gathered value for key %q", key,
				)
			}
			values[key] = v

		case arcanav1.SourceClaimParam:
			v, err := claimParam(key, src.Path, claim)
			if err != nil {
				return nil, err
			}
			values[key] = v

		case arcanav1.SourceTemplate:
			// The context is rebuilt per key because a template may reference
			// an earlier template, whose value only lands during its own pass.
			tmpl, err := template.New(key).Option("missingkey=error").Parse(src.Value)
			if err != nil {
				return nil, fmt.Errorf("key %q: parsing template: %w", key, err)
			}

			var out strings.Builder
			if err := tmpl.Execute(&out, templateContext(md, values)); err != nil {
				return nil, fmt.Errorf("key %q: executing template: %w", key, err)
			}
			values[key] = out.String()

		default:
			return nil, fmt.Errorf("key %q: unknown source %q", key, src.Source)
		}
	}

	return values, nil
}

// claimParam evaluates one path against the claim.
//
// The path has to select exactly one scalar. Relying on AllowMissingKeys alone
// is not enough: a filter selecting nothing succeeds and yields an empty
// string, and a path landing on a map serialises the whole subtree, so either
// mistake would put junk in the Secret instead of failing.
func claimParam(key, path string, claim *unstructured.Unstructured) (string, error) {
	// validate only rules this out when it was told hasClaim, and a nil
	// dereference would take the whole manager down, not just this reconcile.
	if claim == nil {
		return "", fmt.Errorf("internal error: key %q uses claimParam, but no claim was loaded", key)
	}

	// The parser wants the expression in braces, while the spec field holds a
	// bare path. validate has already rejected a path carrying its own.
	jp := jsonpath.New("claimParam")
	jp.AllowMissingKeys(false)
	if err := jp.Parse("{" + path + "}"); err != nil {
		return "", fmt.Errorf("key %q: parsing path %q: %w", key, path, err)
	}

	results, err := jp.FindResults(claim.Object)
	if err != nil {
		return "", fmt.Errorf("key %q: evaluating path %q: %w", key, path, err)
	}

	var found []reflect.Value
	for _, r := range results {
		found = append(found, r...)
	}

	if len(found) != 1 {
		return "", fmt.Errorf("key %q: path %q selected %d values, it has to select exactly one", key, path, len(found))
	}

	if !found[0].IsValid() {
		return "", fmt.Errorf("key %q: path %q selected nothing", key, path)
	}

	// An unstructured claim holds only these scalars, plus maps and slices,
	// which are what the default case rejects.
	switch v := found[0].Interface().(type) {
	case string:
		return v, nil
	case bool, int64, float64:
		return fmt.Sprintf("%v", v), nil
	default:
		return "", fmt.Errorf("key %q: path %q selected a %T, it has to select a scalar", key, path, v)
	}
}

// templateContext is the data a template value renders against: the values
// resolved so far, overlaid with the metadata fields. It is deliberately not
// the result map, which has to come back holding mapping keys and nothing
// else.
//
// Metadata is written last so a mapping key can never shadow it. validate
// rejects a reserved key long before this runs, so this is the second line of
// defence and not the first.
func templateContext(md metadata, values map[string]string) map[string]any {
	ctx := make(map[string]any, len(values)+6)

	for k, v := range values {
		ctx[k] = v
	}

	ctx["ClaimName"] = md.ClaimName
	ctx["ClaimNamespace"] = md.ClaimNamespace
	ctx["ClaimKind"] = md.ClaimKind
	ctx["ClaimAPIVersion"] = md.ClaimAPIVersion
	ctx["InstanceNamespace"] = md.InstanceNamespace
	ctx["ArcanumName"] = md.ArcanumName

	return ctx
}
