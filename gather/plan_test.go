package gather

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func samplePlan() Plan {
	return Plan{
		Namespace: "x-abcd1234-tenant-postgres-poc",
		Entries: []Entry{
			{
				Key:        "USERNAME",
				Kind:       EntryObjectRef,
				APIVersion: "v1",
				ObjectKind: "Secret",
				Name:       "postgres-poc-app",
				DataKey:    "username",
			},
			{
				Key:  "DATABASE",
				Kind: EntryExec,
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":      "postgres",
						"role":     "primary",
						"instance": "postgres-poc",
					},
				},
				Container: "postgres",
				Command:   []string{"psql", "-tAc", "select current_database()"},
			},
		},
	}
}

// The hash names the gather Job. If it moved between reconciles custos would
// tear down and recreate the Job every pass, and since an exec is allowed to
// provision, that could mint a new password every few seconds. The multi-key
// matchLabels map is the part most likely to break this, because Go map
// iteration order is randomised.
func TestPlanHash_IsStableAcrossRuns(t *testing.T) {
	want := samplePlan().Hash()

	for range 200 {
		assert.Equal(t, want, samplePlan().Hash())
	}
}

// Entry order comes from ranging a Go map in the controller, so the hash must
// not depend on it.
func TestPlanHash_IgnoresEntryOrder(t *testing.T) {
	forward := samplePlan()
	reversed := samplePlan()
	reversed.Entries[0], reversed.Entries[1] = reversed.Entries[1], reversed.Entries[0]

	assert.Equal(t, forward.Hash(), reversed.Hash())
}

func TestPlanHash_ChangesWhenAValueChanges(t *testing.T) {
	changed := samplePlan()
	changed.Entries[0].DataKey = "user"

	assert.NotEqual(t, samplePlan().Hash(), changed.Hash())
}

func TestPlanHash_ChangesWhenACommandArgumentChanges(t *testing.T) {
	changed := samplePlan()
	changed.Entries[1].Command = []string{"psql", "-tAc", "select 1"}

	assert.NotEqual(t, samplePlan().Hash(), changed.Hash())
}

// The Job name embeds this, so it has to be usable in one.
func TestPlanHash_IsEightLowercaseHexCharacters(t *testing.T) {
	assert.Regexp(t, `^[0-9a-f]{8}$`, samplePlan().Hash())
}

// TestPlanHash_IsStableAcrossRuns only proves the label map does not make the
// hash move. Dropping the label loop entirely would pass it just as well, and
// then two Arcana pointing at different Pods would share one Job name.
func TestPlanHash_ChangesWhenAPodSelectorLabelChanges(t *testing.T) {
	changed := samplePlan()
	changed.Entries[1].PodSelector.MatchLabels["role"] = "replica"

	assert.NotEqual(t, samplePlan().Hash(), changed.Hash())
}

// Hash has to read matchExpressions too. A selector that differs only there
// still picks a different Pod.
func TestPlanHash_ChangesWhenAMatchExpressionChanges(t *testing.T) {
	changed := samplePlan()
	changed.Entries[1].PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
		Key:      "role",
		Operator: metav1.LabelSelectorOpIn,
		Values:   []string{"primary"},
	}}

	assert.NotEqual(t, samplePlan().Hash(), changed.Hash())
}

// The namespace is half of what the Job does, and two instances of the same
// service otherwise hold identical plans.
func TestPlanHash_ChangesWhenTheNamespaceChanges(t *testing.T) {
	changed := samplePlan()
	changed.Namespace = "x-99887766-tenant-postgres-prod"

	assert.NotEqual(t, samplePlan().Hash(), changed.Hash())
}

// The controller hashes before it knows whether there is anything to gather,
// so an empty plan has to produce a name rather than trip over the nil slice.
func TestPlanHash_HandlesAPlanWithNoEntries(t *testing.T) {
	assert.Regexp(t, `^[0-9a-f]{8}$`, Plan{}.Hash())
}

func TestPlanMarshal_SortsEntriesByKey(t *testing.T) {
	b, err := samplePlan().Marshal()
	require.NoError(t, err)

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Entries, 2)
	assert.Equal(t, "DATABASE", got.Entries[0].Key)
	assert.Equal(t, "USERNAME", got.Entries[1].Key)
}

func TestPlanRoundTrip_PreservesEveryField(t *testing.T) {
	b, err := samplePlan().Marshal()
	require.NoError(t, err)

	got, err := Unmarshal(b)
	require.NoError(t, err)

	want := samplePlan()
	assert.Equal(t, want.Namespace, got.Namespace)
	assert.ElementsMatch(t, want.Entries, got.Entries)
}

// The controller builds Entries by ranging a map and keeps hold of that slice.
// Sorting it underneath would make the order the caller sees depend on whether
// Marshal had run yet.
func TestPlanMarshal_LeavesTheCallersSliceAlone(t *testing.T) {
	plan := samplePlan()

	_, err := plan.Marshal()
	require.NoError(t, err)

	assert.Equal(t, "USERNAME", plan.Entries[0].Key)
	assert.Equal(t, "DATABASE", plan.Entries[1].Key)
}

// Reading the order back through Unmarshal cannot say which of the two sorted,
// and it is the bytes in the ConfigMap that a human reads.
func TestPlanMarshal_WritesEntriesInKeyOrder(t *testing.T) {
	b, err := samplePlan().Marshal()
	require.NoError(t, err)

	assert.Less(t, strings.Index(string(b), `"DATABASE"`), strings.Index(string(b), `"USERNAME"`))
}

// The Job resolves entries in the namespace the plan names, so a plan that
// loses it on the way through the ConfigMap is a Job with nowhere to write.
func TestPlanMarshal_KeepsTheNamespace(t *testing.T) {
	b, err := samplePlan().Marshal()
	require.NoError(t, err)

	assert.Contains(t, string(b), "x-abcd1234-tenant-postgres-poc")
}
