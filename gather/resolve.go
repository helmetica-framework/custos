package gather

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// secretKind is the one kind whose data needs decoding.
const secretKind = "Secret"

// Executor runs a command in a pod and returns its stdout. It is an interface
// because neither the fake client nor envtest can exec, so the resolver would
// otherwise be untestable.
type Executor interface {
	Exec(ctx context.Context, namespace, pod, container string, command []string) (string, error)
}

// Resolver resolves a plan inside one namespace, with the token of the
// ServiceAccount the Job runs as. It has no view of anything outside that
// namespace and does not need one.
type Resolver struct {
	Client   client.Client
	Executor Executor
	// PodWait is how long to wait for an exec's pod to become ready. Zero
	// looks once and fails, which is what a unit test and a hand-run gather
	// want. The Job sets it, because a helm install routinely starts the
	// gather before the service it reads from is up.
	PodWait time.Duration
	// PodPoll is how often to look while waiting. Zero means the default.
	PodPoll time.Duration
}

// podWait is how long this entry gets to wait for its pod: what the Arcanum
// asked for, or the run's own default when it asked for nothing.
func (r *Resolver) podWait(entry Entry) time.Duration {
	if entry.PodWait != nil {
		return entry.PodWait.Duration
	}

	return r.PodWait
}

// Resolve returns one value per plan entry, keyed by Entry.Key.
//
// Any failure fails the whole run. A half-resolved map must never reach the
// render stage, because a template referring to a key that is missing would
// otherwise quietly render an empty value into a credential.
func (r *Resolver) Resolve(ctx context.Context, plan Plan) (map[string]string, error) {
	values := make(map[string]string, len(plan.Entries))

	for _, entry := range plan.Entries {
		// The key, never the value. Everything this loop produces is a
		// credential, and the Job's log is not a place to put one.
		slog.Info("resolving", "key", entry.Key, "kind", entry.Kind)

		var (
			value string
			err   error
		)

		switch entry.Kind {
		case EntryObjectRef:
			value, err = r.resolveObjectRef(ctx, plan.Namespace, entry)
		case EntryExec:
			value, err = r.resolveExec(ctx, plan.Namespace, entry)
		default:
			err = fmt.Errorf("key %q: unknown entry kind %q", entry.Key, entry.Kind)
		}

		if err != nil {
			slog.Error("could not resolve", "key", entry.Key, "error", err)

			return nil, err
		}

		values[entry.Key] = value
	}

	slog.Info("resolved every entry", "values", len(values))

	return values, nil
}

// resolveObjectRef reads one value out of one object in the namespace, either
// a data key or whatever a JSONPath selects.
//
// Every error names the entry key and the object and never the value. These
// messages end up on the Arcanum's status, which anyone who can read the
// claim can read too.
func (r *Resolver) resolveObjectRef(ctx context.Context, namespace string, entry Entry) (string, error) {
	slog.Info("reading object",
		"key", entry.Key, "apiVersion", entry.APIVersion, "kind", entry.ObjectKind, "name", entry.Name)

	gv, err := schema.ParseGroupVersion(entry.APIVersion)
	if err != nil {
		return "", fmt.Errorf("key %q: parsing apiVersion %q: %w", entry.Key, entry.APIVersion, err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(entry.ObjectKind))

	err = r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: entry.Name}, obj)
	if err != nil {
		return "", fmt.Errorf("key %q: getting %s %q: %w", entry.Key, entry.ObjectKind, entry.Name, err)
	}

	if entry.DataKey != "" {
		return dataKeyValue(entry, obj)
	}

	return stringAtPath(entry, obj)
}

// dataKeyValue reads one entry out of a Secret's or ConfigMap's data.
func dataKeyValue(entry Entry, obj *unstructured.Unstructured) (string, error) {
	raw, found, err := unstructured.NestedString(obj.Object, "data", entry.DataKey)
	if err != nil {
		return "", fmt.Errorf("key %q: reading data %q of %s %q: %w",
			entry.Key, entry.DataKey, entry.ObjectKind, entry.Name, err)
	}

	if !found {
		return "", fmt.Errorf("key %q: %s %q has no data key %q",
			entry.Key, entry.ObjectKind, entry.Name, entry.DataKey)
	}

	// Read as unstructured, a Secret still holds the base64 the API returns,
	// while a ConfigMap holds the value itself. The kind decides, not a trial
	// decode: plenty of plaintext is valid base64, so a ConfigMap value of
	// "aGVsbG8=" would come back as mojibake.
	if entry.ObjectKind != secretKind {
		return raw, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("key %q: decoding data %q of Secret %q: %w",
			entry.Key, entry.DataKey, entry.Name, err)
	}

	return string(decoded), nil
}

// stringAtPath evaluates entry.Path against the object and returns the single
// scalar it selects.
//
// Paths arrive in either spelling, since only a claimParam path is validated
// bare. A bare path does not fail to parse, it comes back as its own literal
// text, so both spellings are normalised to one.
func stringAtPath(entry Entry, obj *unstructured.Unstructured) (string, error) {
	expr := entry.Path
	if !strings.HasPrefix(expr, "{") {
		expr = "{" + expr + "}"
	}

	jp := jsonpath.New("objectRef")

	// Otherwise a typo selects nothing, resolves to an empty string, and
	// writes an empty credential with no error anywhere.
	jp.AllowMissingKeys(false)

	if err := jp.Parse(expr); err != nil {
		return "", fmt.Errorf("key %q: parsing path %q: %w", entry.Key, entry.Path, err)
	}

	results, err := jp.FindResults(obj.Object)
	if err != nil {
		return "", fmt.Errorf("key %q: evaluating path %q against %s %q: %w",
			entry.Key, entry.Path, entry.ObjectKind, entry.Name, err)
	}

	var found []reflect.Value
	for _, result := range results {
		found = append(found, result...)
	}

	if len(found) != 1 {
		return "", fmt.Errorf("key %q: path %q selected %d values, it has to select exactly one",
			entry.Key, entry.Path, len(found))
	}

	if !found[0].IsValid() {
		return "", fmt.Errorf("key %q: path %q selected nothing", entry.Key, entry.Path)
	}

	// An unstructured object holds only these scalars, plus the maps and
	// slices the default case rejects.
	switch v := found[0].Interface().(type) {
	case string:
		return v, nil
	case bool, int64, float64:
		return fmt.Sprintf("%v", v), nil
	default:
		return "", fmt.Errorf("key %q: path %q selected a %T, it has to select a scalar",
			entry.Key, entry.Path, v)
	}
}

// resolveExec runs the entry's command in a pod matching its selector and
// returns stdout with the trailing newline trimmed, since almost every CLI
// adds one and almost no credential wants it.
func (r *Resolver) resolveExec(ctx context.Context, namespace string, entry Entry) (string, error) {
	pod, err := r.selectPod(ctx, namespace, entry.PodSelector, r.podWait(entry))
	if err != nil {
		return "", fmt.Errorf("key %q: %w", entry.Key, err)
	}

	name := pod.GetName()
	container := execContainer(pod, entry.Container)

	// The pod and the container, never the command. A provisioning exec puts
	// the value it is setting in its own arguments.
	slog.Info("running command", "key", entry.Key, "pod", name, "container", container)

	out, err := r.Executor.Exec(ctx, namespace, name, container, entry.Command)
	if err != nil {
		// The executor keeps stdout out of its errors, since stdout is the
		// value. Naming the pod is safe and is the first thing anyone will
		// want when a command fails.
		return "", fmt.Errorf("key %q: running command in pod %q: %w", entry.Key, name, err)
	}

	return strings.TrimRight(out, "\n"), nil
}
