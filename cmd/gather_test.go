package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/helmetica-framework/custos/gather"
)

// The Job passes all five of these. A renamed flag turns into a Job that
// fails at startup, which no unit test elsewhere would catch.
func TestGatherCommand_HasTheFlagsTheJobPasses(t *testing.T) {
	for _, name := range []string{"plan", "secret", "result", "arcanum", "arcanum-uid"} {
		assert.NotNil(t, gatherCmd.Flags().Lookup(name), "missing flag %q", name)
	}
}

// The default has to match where the controller mounts the plan volume, so a
// Job that omits the flag still finds its plan.
func TestGatherCommand_PlanDefaultsToTheMountPath(t *testing.T) {
	flag := gatherCmd.Flags().Lookup("plan")

	require.NotNil(t, flag)
	assert.Equal(t, "/etc/custos/plan.json", flag.DefValue)
}

func TestGatherCommand_IsRegisteredOnTheRoot(t *testing.T) {
	var found bool
	for _, c := range RootCmd.Commands() {
		if c.Name() == "gather" {
			found = true
		}
	}

	assert.True(t, found, "custos gather is not reachable from the root command")
}

// The flags are package level, so a test that sets them puts them back.
func withGatherFlags(t *testing.T, secret, arcanum, uid string) {
	t.Helper()

	oldSecret, oldArcanum, oldUID := secretName, arcanumName, arcanumUID

	secretName, arcanumName, arcanumUID = secret, arcanum, uid

	t.Cleanup(func() {
		secretName, arcanumName, arcanumUID = oldSecret, oldArcanum, oldUID
	})
}

func gatherClient() client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).Build()
}

const gatherTestNamespace = "x-abcd1234-tenant-postgres-poc"

// The label is the whole reason the controller can read this Secret back. Its
// cache is restricted to Secrets carrying it, so an unlabelled one is
// NotFound however often the controller looks.
func TestApplyGathered_WritesTheValuesUnderTheArcanumLabel(t *testing.T) {
	withGatherFlags(t, "sample-gathered", "sample", "")
	c := gatherClient()
	ctx := context.Background()

	err := applyGathered(ctx, c, gatherTestNamespace, map[string]string{"USERNAME": "app"})

	require.NoError(t, err)

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "sample-gathered", Namespace: gatherTestNamespace,
	}, got))

	assert.Equal(t, "sample", got.Labels[gather.ArcanumNameLabel])

	// Data holds plain bytes. The base64 in a Secret belongs to the wire
	// format, and the serializer puts it there.
	assert.Equal(t, map[string][]byte{"USERNAME": []byte("app")}, got.Data)
}

// Deleting the Arcanum has to take the gathered credentials with it, and the
// Job is the only process that ever writes this Secret.
func TestApplyGathered_IsOwnedByTheArcanum(t *testing.T) {
	withGatherFlags(t, "sample-gathered", "sample", "11111111-2222-3333-4444-555555555555")
	c := gatherClient()
	ctx := context.Background()

	require.NoError(t, applyGathered(ctx, c, gatherTestNamespace, map[string]string{"USERNAME": "app"}))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "sample-gathered", Namespace: gatherTestNamespace,
	}, got))

	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, "Arcanum", got.OwnerReferences[0].Kind)
	assert.Equal(t, "sample", got.OwnerReferences[0].Name)
	assert.Equal(t, types.UID("11111111-2222-3333-4444-555555555555"), got.OwnerReferences[0].UID)
	assert.True(t, *got.OwnerReferences[0].Controller)

	// Setting it needs update on the owner's finalizers subresource, which
	// the admin ClusterRole does not cover for a custom kind.
	assert.Nil(t, got.OwnerReferences[0].BlockOwnerDeletion,
		"blockOwnerDeletion would have the API server reject the whole write")
}

// An owner reference with an empty UID is rejected outright, so a run without
// one has to write the Secret with no reference rather than a broken one.
func TestApplyGathered_WithoutAUIDWritesNoOwnerReference(t *testing.T) {
	withGatherFlags(t, "sample-gathered", "sample", "")
	c := gatherClient()
	ctx := context.Background()

	require.NoError(t, applyGathered(ctx, c, gatherTestNamespace, map[string]string{"USERNAME": "app"}))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "sample-gathered", Namespace: gatherTestNamespace,
	}, got))

	assert.Empty(t, got.OwnerReferences)
}
