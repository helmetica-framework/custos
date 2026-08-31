package controllers

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
)

func testMetadata() metadata {
	return metadata{
		ClaimName:         "postgres-poc",
		ClaimNamespace:    "tenant",
		ClaimKind:         "PostgreSQL",
		ClaimAPIVersion:   "v6.postgres.helmetica-bundles.io/v1",
		InstanceNamespace: "x-abcd1234-tenant-postgres-poc",
		ArcanumName:       "credentials",
	}
}

func TestRenderMetadata_SubstitutesClaimName(t *testing.T) {
	got, err := renderMetadata("{{.ClaimName}}-app", testMetadata())

	require.NoError(t, err)
	assert.Equal(t, "postgres-poc-app", got)
}

// A name that references something outside the metadata context is a typo the
// author wants to hear about, not an empty string silently baked into a
// lookup that will then fail with a confusing "secret -app not found".
func TestRenderMetadata_UnknownFieldIsAnError(t *testing.T) {
	_, err := renderMetadata("{{.NoSuchThing}}-app", testMetadata())

	require.Error(t, err)
}

func TestTemplateRefs_FindsPlainReferences(t *testing.T) {
	got, err := templateRefs("postgres://{{.USER}}:{{.PASS}}@{{.HOST}}/{{.DB}}")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"USER", "PASS", "HOST", "DB"}, got)
}

// A reference inside a control structure is still a reference. Pattern
// matching the string would miss the condition and find only BODY.
func TestTemplateRefs_FindsReferencesInsideBranches(t *testing.T) {
	got, err := templateRefs(`{{if .TLS}}{{.BODY}}{{else}}{{.FALLBACK}}{{end}}`)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"TLS", "BODY", "FALLBACK"}, got)
}

// An escaped brace is literal output, not a reference. Pattern matching would
// see ".NOT_A_REF" here and invent a dependency that does not exist.
func TestTemplateRefs_IgnoresEscapedBraces(t *testing.T) {
	got, err := templateRefs(`{{"{{"}}.NOT_A_REF{{"}}"}}`)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestValidate_AcceptsAWellFormedMapping(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"HOST": {Source: arcanav1.SourceConst, Value: "postgres-poc-rw"},
		"DB":   {Source: arcanav1.SourceClaimParam, Path: ".spec.parameters.service.database"},
		"URL":  {Source: arcanav1.SourceTemplate, Value: "postgres://{{.HOST}}/{{.DB}}"},
	}

	require.NoError(t, validate(mapping, true))
}

func TestValidate_RejectsAReservedName(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"ClaimName": {Source: arcanav1.SourceConst, Value: "nope"},
	}

	err := validate(mapping, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ClaimName")
}

func TestValidate_RejectsAnUnknownTemplateReference(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"URL":  {Source: arcanav1.SourceTemplate, Value: "{{.HOST}}/{{.MISSING}}"},
		"HOST": {Source: arcanav1.SourceConst, Value: "h"},
	}

	err := validate(mapping, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING")
}

// A template may reference the metadata context, which is not a mapping key.
// Without this the unknown-reference check would reject every use of
// {{.ClaimName}} in a template value.
func TestValidate_AcceptsAMetadataReferenceInATemplate(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"URL": {Source: arcanav1.SourceTemplate, Value: "https://{{.ClaimName}}.example"},
	}

	require.NoError(t, validate(mapping, true))
}

func TestValidate_RejectsACycle(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"A": {Source: arcanav1.SourceTemplate, Value: "{{.B}}"},
		"B": {Source: arcanav1.SourceTemplate, Value: "{{.A}}"},
	}

	err := validate(mapping, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
}

// A claimParam in a namespace with no claim can never resolve. Failing here
// costs nothing; failing later costs a gather Job that may have provisioned
// something.
func TestValidate_RejectsClaimParamWithoutAClaim(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"DB": {Source: arcanav1.SourceClaimParam, Path: ".spec.parameters.service.database"},
	}

	err := validate(mapping, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "claimParam")
}

func TestRenderOrder_PutsDependenciesFirst(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"URL":  {Source: arcanav1.SourceTemplate, Value: "{{.HOST}}/{{.DB}}"},
		"HOST": {Source: arcanav1.SourceConst, Value: "h"},
		"DB":   {Source: arcanav1.SourceTemplate, Value: "{{.NAME}}"},
		"NAME": {Source: arcanav1.SourceConst, Value: "n"},
	}

	order, err := renderOrder(mapping)

	require.NoError(t, err)
	assert.Equal(t, []string{"HOST", "NAME", "DB", "URL"}, order,
		"non-templates come first in sorted order, then templates in dependency order")
}

// Go map iteration is randomised. If renderOrder leaked that randomness the
// rendered output would still be correct but the plan hash built from it
// would not be, so the order has to be pinned.
func TestRenderOrder_IsDeterministic(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"A": {Source: arcanav1.SourceConst, Value: "a"},
		"B": {Source: arcanav1.SourceConst, Value: "b"},
		"C": {Source: arcanav1.SourceConst, Value: "c"},
		"D": {Source: arcanav1.SourceConst, Value: "d"},
		"E": {Source: arcanav1.SourceTemplate, Value: "{{.A}}{{.B}}"},
		"F": {Source: arcanav1.SourceTemplate, Value: "{{.E}}{{.C}}"},
	}

	first, err := renderOrder(mapping)
	require.NoError(t, err)

	for range 50 {
		again, err := renderOrder(mapping)
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
}

func claimObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v6.postgres.helmetica-bundles.io/v1",
		"kind":       "PostgreSQL",
		"metadata":   map[string]any{"name": "postgres-poc", "namespace": "tenant"},
		"spec": map[string]any{
			"parameters": map[string]any{
				"service": map[string]any{"database": "poc", "port": int64(5432)},
			},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Synced", "status": "True"},
			},
		},
	}}
}

func TestRender_ResolvesEverySourceKind(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"HOST":     {Source: arcanav1.SourceConst, Value: "postgres-poc-rw"},
		"PORT":     {Source: arcanav1.SourceConst, Value: "5432"},
		"USERNAME": {Source: arcanav1.SourceObjectRef, Kind: "Secret", Name: "{{.ClaimName}}-app", Key: "username"},
		"PASSWORD": {Source: arcanav1.SourceObjectRef, Kind: "Secret", Name: "{{.ClaimName}}-app", Key: "password"},
		"DATABASE": {Source: arcanav1.SourceClaimParam, Path: ".spec.parameters.service.database"},
		"URL": {
			Source: arcanav1.SourceTemplate,
			Value:  "postgres://{{.USERNAME}}:{{.PASSWORD}}@{{.HOST}}:{{.PORT}}/{{.DATABASE}}",
		},
	}
	gathered := map[string]string{"USERNAME": "app", "PASSWORD": "hunter2"}

	got, err := render(mapping, testMetadata(), gathered, claimObject())

	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"HOST":     "postgres-poc-rw",
		"PORT":     "5432",
		"USERNAME": "app",
		"PASSWORD": "hunter2",
		"DATABASE": "poc",
		"URL":      "postgres://app:hunter2@postgres-poc-rw:5432/poc",
	}, got)
}

func TestRender_TemplateSeesTheMetadataContext(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"ENDPOINT": {Source: arcanav1.SourceTemplate, Value: "{{.ClaimName}}.{{.ClaimNamespace}}.svc"},
	}

	got, err := render(mapping, testMetadata(), nil, nil)

	require.NoError(t, err)
	assert.Equal(t, "postgres-poc.tenant.svc", got["ENDPOINT"])
}

// A template may build on another template. This is the case the topological
// sort exists for: URL cannot render until DSN has.
func TestRender_TemplateBuiltOnAnotherTemplate(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"HOST": {Source: arcanav1.SourceConst, Value: "h"},
		"DSN":  {Source: arcanav1.SourceTemplate, Value: "{{.HOST}}:5432"},
		"URL":  {Source: arcanav1.SourceTemplate, Value: "postgres://{{.DSN}}/db"},
	}

	got, err := render(mapping, testMetadata(), nil, nil)

	require.NoError(t, err)
	assert.Equal(t, "postgres://h:5432/db", got["URL"])
}

func TestRender_MissingClaimPathIsAnError(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"DB": {Source: arcanav1.SourceClaimParam, Path: ".spec.parameters.service.nothingHere"},
	}

	_, err := render(mapping, testMetadata(), nil, claimObject())

	require.Error(t, err)
}

// Every metadata field is a name a mapping key could shadow, so adding a field
// without adding it to the reserved set would silently allow that.
func TestReservedNames_CoversEveryMetadataField(t *testing.T) {
	assert.Len(t, reservedNames(), reflect.TypeOf(metadata{}).NumField())
}

// A block body is parsed into an associated template, which the main tree does
// not reach. A reference missed here is an edge missing from the graph, so the
// key renders before the one it depends on.
func TestTemplateRefs_FindsReferencesInsideABlock(t *testing.T) {
	got, err := templateRefs(`{{block "inner" .}}{{.HOST}}{{end}}`)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"HOST"}, got)
}

func TestTemplateRefs_FindsAChainedReference(t *testing.T) {
	got, err := templateRefs(`{{(.A).B}}{{.C}}`)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"A", "C"}, got)
}

// range rebinds the dot, so .name belongs to whatever .HOSTS produced. Reading
// it as a top level name would invent a dependency on a key nobody declared.
func TestTemplateRefs_IgnoresFieldsUnderAReboundDot(t *testing.T) {
	got, err := templateRefs(`{{range .HOSTS}}{{.name}}{{end}}`)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"HOSTS"}, got)
}

func TestValidate_RejectsAnUnknownReferenceInsideABlock(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"URL": {Source: arcanav1.SourceTemplate, Value: `{{block "inner" .}}{{.MISSING}}{{end}}`},
	}

	err := validate(mapping, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING")
}

func TestValidate_AcceptsAFieldUnderAReboundDot(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"HOSTS": {Source: arcanav1.SourceConst, Value: "h"},
		"OUT":   {Source: arcanav1.SourceTemplate, Value: `{{range .HOSTS}}{{.name}}{{end}}`},
	}

	require.NoError(t, validate(mapping, true))
}

// A path carrying its own braces closes the expression render wraps it in and
// opens another, which serialises the entire claim into the Secret. It is also
// the spelling kubectl uses, so it is an easy mistake to make.
func TestValidate_RejectsBracesInAClaimParamPath(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"DB": {Source: arcanav1.SourceClaimParam, Path: "{.spec.parameters.service.database}"},
	}

	err := validate(mapping, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "braces")
}

func TestRender_ClaimParamReadsANonStringScalar(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"PORT": {Source: arcanav1.SourceClaimParam, Path: ".spec.parameters.service.port"},
	}

	got, err := render(mapping, testMetadata(), nil, claimObject())

	require.NoError(t, err)
	assert.Equal(t, "5432", got["PORT"])
}

// AllowMissingKeys catches a missing map key but not a filter that matches
// nothing, which succeeds and yields an empty string.
func TestRender_ClaimFilterMatchingNothingIsAnError(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"READY": {Source: arcanav1.SourceClaimParam, Path: `.status.conditions[?(@.type=="Ready")].status`},
	}

	_, err := render(mapping, testMetadata(), nil, claimObject())

	require.Error(t, err)
}

// .spec.parameters is one typo away from .spec.parameters.service.database and
// would otherwise dump the whole subtree into the Secret as JSON.
func TestRender_ClaimPathSelectingASubtreeIsAnError(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"DB": {Source: arcanav1.SourceClaimParam, Path: ".spec.parameters"},
	}

	_, err := render(mapping, testMetadata(), nil, claimObject())

	require.Error(t, err)
}

// validate rejects a reserved key before render ever sees one, so this pins
// the fallback: metadata is not shadowable even if a caller skips validate.
func TestTemplateContext_MetadataOutranksAMappingKey(t *testing.T) {
	ctx := templateContext(testMetadata(), map[string]string{"ClaimName": "shadow"})

	assert.Equal(t, "postgres-poc", ctx["ClaimName"])
}
