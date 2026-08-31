package controllers

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
	"github.com/helmetica-framework/custos/gather"
)

func gatherMapping() map[string]arcanav1.ValueSource {
	return map[string]arcanav1.ValueSource{
		"HOST": {Source: arcanav1.SourceConst, Value: "postgres-poc-rw"},
		"USERNAME": {
			Source:     arcanav1.SourceObjectRef,
			APIVersion: "v1",
			Kind:       "Secret",
			Name:       "{{.ClaimName}}-app",
			Key:        "username",
		},
		"DATABASE": {
			Source:      arcanav1.SourceExec,
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}},
			Container:   "postgres",
			Command:     []string{"psql", "-tAc", "select current_database()"},
		},
		"URL": {Source: arcanav1.SourceTemplate, Value: "{{.HOST}}/{{.DATABASE}}"},
	}
}

// Only objectRef and exec entries reach the Job. Sending const or template
// entries would put values in the Job's ConfigMap that never needed to leave
// the controller.
func TestBuildPlan_IncludesOnlyGatherableEntries(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())

	require.NoError(t, err)
	require.Len(t, plan.Entries, 2)

	keys := []string{plan.Entries[0].Key, plan.Entries[1].Key}
	assert.ElementsMatch(t, []string{"USERNAME", "DATABASE"}, keys)
}

func TestBuildPlan_RendersTheMetadataTemplateInAName(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)

	var entry gather.Entry
	for _, e := range plan.Entries {
		if e.Key == "USERNAME" {
			entry = e
		}
	}

	assert.Equal(t, "postgres-poc-app", entry.Name,
		"the Job gets a literal name, never a template")
	assert.Equal(t, "Secret", entry.ObjectKind)
	assert.Equal(t, "username", entry.DataKey)
}

func TestBuildPlan_RendersTheMetadataTemplateInACommand(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"DB": {
			Source:      arcanav1.SourceExec,
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pg"}},
			Command:     []string{"sh", "-c", "echo {{.ClaimName}}"},
		},
	}

	plan, err := buildPlan(mapping, testMetadata())

	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", "echo postgres-poc"}, plan.Entries[0].Command)
}

func TestBuildPlan_SetsTheInstanceNamespace(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())

	require.NoError(t, err)
	assert.Equal(t, "x-abcd1234-tenant-postgres-poc", plan.Namespace)
}

func TestNeedsGather_TrueWithAnObjectRef(t *testing.T) {
	assert.True(t, needsGather(gatherMapping()))
}

// A mapping custos can resolve on its own must not spend a Job. This is what
// keeps a simple Arcanum synchronous.
func TestNeedsGather_FalseWithoutObjectRefOrExec(t *testing.T) {
	mapping := map[string]arcanav1.ValueSource{
		"HOST": {Source: arcanav1.SourceConst, Value: "h"},
		"DB":   {Source: arcanav1.SourceClaimParam, Path: ".spec.db"},
		"URL":  {Source: arcanav1.SourceTemplate, Value: "{{.HOST}}/{{.DB}}"},
	}

	assert.False(t, needsGather(mapping))
}

// The refresh annotation is the on-demand trigger. Changing its value has to
// move the hash, or the Job name would not change and nothing would re-run.
func TestGatherHash_ChangesWithTheRefreshToken(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)

	assert.NotEqual(t, gatherHash(plan, ""), gatherHash(plan, "2026-08-28T10:00:00Z"))
}

func TestGatherHash_IsStableForTheSameInputs(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)

	want := gatherHash(plan, "token")
	for range 100 {
		again, err := buildPlan(gatherMapping(), testMetadata())
		require.NoError(t, err)
		assert.Equal(t, want, gatherHash(again, "token"))
	}
}

// Names go into the Job's metadata.name, which is limited to 63 characters
// and must be a DNS label.
func TestGatherJobName_IsADNSLabel(t *testing.T) {
	name := gatherJobName("credentials", "a1b2c3d4")

	assert.Equal(t, "credentials-gather-a1b2c3d4", name)
	assert.LessOrEqual(t, len(name), 63)
}

func TestGatherJob_RunsAsInstanceAdminWithNoRetries(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)
	m, _ := newManager()
	m.GatherImage = "ghcr.io/helmetica-framework/custos:v1"

	job := m.gatherJob(arcanum(1), plan, "a1b2c3d4")

	require.NotNil(t, job.Spec)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit,
		"an exec may provision, so a silent retry could mint a second password")
	require.NotNil(t, job.Spec.Template.Spec)
	assert.Equal(t, "instance-admin", *job.Spec.Template.Spec.ServiceAccountName)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "ghcr.io/helmetica-framework/custos:v1",
		*job.Spec.Template.Spec.Containers[0].Image)
}

// The Job cannot find the Arcanum on its own, so anything it has to stamp on
// what it writes travels in the args. Without the name the gathered Secret
// misses the label the manager cache selects on, and the controller reads it
// back as NotFound.
func TestGatherJob_PassesTheArcanumToTheBinary(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)
	m, _ := newManager()

	job := m.gatherJob(arcanum(1), plan, "a1b2c3d4")

	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	args := job.Spec.Template.Spec.Containers[0].Args

	assert.Equal(t, "sample", argValue(args, "--arcanum"))
	assert.Equal(t, string(arcanumUID), argValue(args, "--arcanum-uid"))
}

// argValue returns what follows flag in a container's args.
func argValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// Gather Jobs left over from a previous plan are deleted by listing on this
// label. Without it there is no way to find them.
func TestGatherJob_CarriesTheArcanumLabel(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)
	m, _ := newManager()

	job := m.gatherJob(arcanum(1), plan, "a1b2c3d4")

	assert.Equal(t, "sample", job.Labels[arcanumNameLabel])
}

func TestGatherJob_IsOwnedByTheArcanum(t *testing.T) {
	plan, err := buildPlan(gatherMapping(), testMetadata())
	require.NoError(t, err)
	m, _ := newManager()

	job := m.gatherJob(arcanum(1), plan, "a1b2c3d4")

	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, "sample", *job.OwnerReferences[0].Name)
	assert.Equal(t, arcanumUID, *job.OwnerReferences[0].UID)
	assert.True(t, *job.OwnerReferences[0].Controller)
}

// An Arcanum name can already be 63 characters, so any suffix pushes it over
// and the API server rejects the name rather than shortening it for us.
func TestGatherJobName_TruncatesALongArcanumName(t *testing.T) {
	name := gatherJobName(strings.Repeat("a", 63), "a1b2c3d4")

	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, name)
	assert.Contains(t, name, "gather-a1b2c3d4",
		"the suffix says which of the four objects this is, so it survives")
}

// Truncating alone would give two Arcana that share a long prefix one name.
// In a shared namespace that is two services overwriting each other's
// credentials in a single Secret.
func TestGatherNames_DoNotCollideOnASharedPrefix(t *testing.T) {
	prefix := strings.Repeat("a", 60)

	assert.NotEqual(t,
		gatheredSecretName(prefix+"one"),
		gatheredSecretName(prefix+"two"))
}

// The four names have to differ, or the plan ConfigMap and the result
// ConfigMap are the same object and the Job overwrites its own input.
func TestGatherNames_AreDistinct(t *testing.T) {
	names := map[string]bool{
		gatherJobName("credentials", "a1b2c3d4"): true,
		gatherPlanConfigMapName("credentials"):   true,
		gatherResultConfigMapName("credentials"): true,
		gatheredSecretName("credentials"):        true,
	}

	assert.Len(t, names, 4)
}
