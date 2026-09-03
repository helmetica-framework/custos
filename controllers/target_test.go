package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
)

func credentials() map[string]string {
	return map[string]string{"HOST": "postgres-poc-rw", "PASSWORD": "hunter2"}
}

func targetKey(ns string) types.NamespacedName {
	return types.NamespacedName{Name: "postgres-poc-credentials", Namespace: ns}
}

func arcanumWithTarget(generation int64) *arcanav1.Arcanum {
	a := arcanum(generation)
	a.Spec.Target = arcanav1.TargetSpec{Name: "postgres-poc-credentials"}
	return a
}

func secretKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: ns}
}

// renamedTo is an Arcanum whose target has been pointed at a new name, with
// the status still recording where the last pass wrote.
func renamedTo(name, oldNamespace string) *arcanav1.Arcanum {
	a := arcanum(1)
	a.Spec.Target = arcanav1.TargetSpec{Name: name}
	a.Status.SecretName = "postgres-poc-credentials"
	a.Status.SecretNamespace = oldNamespace

	return a
}

func TestApplyTargetSecret_WritesTheData(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))

	// Data holds plain bytes. The base64 in a Secret belongs to the wire
	// format, and the serializer puts it there.
	assert.Equal(t, []byte("postgres-poc-rw"), got.Data["HOST"])
	assert.Equal(t, []byte("hunter2"), got.Data["PASSWORD"])
	assert.Equal(t, corev1.SecretTypeOpaque, got.Type)
}

// The labels are how the finalizer knows the Secret is ours and how the
// controller's cache selector sees it at all.
func TestApplyTargetSecret_StampsOwnershipLabels(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Equal(t, "sample", got.Labels[arcanumNameLabel])
	assert.Equal(t, "default", got.Labels[arcanumNamespaceLabel])
	assert.Equal(t, string(arcanumUID), got.Labels[arcanumUIDLabel])
}

// Owner references cannot cross namespaces, so only the local case gets one.
func TestApplyTargetSecret_LocalTargetGetsAnOwnerReference(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "default", true, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("default"), got))
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, arcanumUID, got.OwnerReferences[0].UID)
}

func TestApplyTargetSecret_RemoteTargetGetsNoOwnerReference(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Empty(t, got.OwnerReferences,
		"an owner reference across namespaces would make the garbage collector delete the Secret")
}

// This is the guard that stops an Arcanum from taking over a Secret the
// tenant already had at that name.
func TestApplyTargetSecret_RefusesAForeignSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-credentials", Namespace: "tenant"},
		Data:       map[string][]byte{"KEEP": []byte("me")},
	}
	m, c := newManager(arcanumWithTarget(1), existing)
	ctx := context.Background()

	err := m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials())

	require.Error(t, err)
	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Equal(t, map[string][]byte{"KEEP": []byte("me")}, got.Data,
		"a Secret custos did not create must be left exactly as it was")
}

func TestApplyTargetSecret_UpdatesItsOwnSecret(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()
	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	rotated := map[string]string{"HOST": "postgres-poc-rw", "PASSWORD": "hunter3"}
	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, rotated))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Equal(t, []byte("hunter3"), got.Data["PASSWORD"])
}

// Renaming spec.target.name has to take the old Secret with it. Leaving it
// behind would keep serving credentials from a name nothing points at any
// more, and no later pass would find it: the status has moved on to the new
// name, which is the only handle cleanup has.
func TestApplyTargetSecret_RenameRemovesTheOldSecret(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	renamed := renamedTo("postgres-poc-renamed", "tenant")
	require.NoError(t, m.applyTargetSecret(ctx, renamed, "tenant", false, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, secretKey("tenant", "postgres-poc-renamed"), got))
	assert.Equal(t, []byte("hunter2"), got.Data["PASSWORD"])

	err := c.Get(ctx, targetKey("tenant"), &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "the Secret under the old name must not be left behind")
}

// A rename that cannot be written must not take the credentials with it. The
// new name being taken is exactly the case a retry cannot fix, so deleting
// first leaves consumers with nothing and no pass that can bring it back.
func TestApplyTargetSecret_RenameOntoAForeignNameKeepsTheOldSecret(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-renamed", Namespace: "tenant"},
		Data:       map[string][]byte{"KEEP": []byte("me")},
	}
	m, c := newManager(arcanumWithTarget(1), foreign)
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	renamed := renamedTo("postgres-poc-renamed", "tenant")

	err := m.applyTargetSecret(ctx, renamed, "tenant", false, credentials())

	require.Error(t, err, "the new name belongs to someone else, so the rename cannot go through")

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got),
		"the credentials consumers are live on must survive a rename that failed")
	assert.Equal(t, []byte("hunter2"), got.Data["PASSWORD"])

	still := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, secretKey("tenant", "postgres-poc-renamed"), still))
	assert.Equal(t, map[string][]byte{"KEEP": []byte("me")}, still.Data,
		"the foreign Secret must be left exactly as it was")
}

// The target namespace is not settable and follows the claim annotation, so
// adding or correcting that annotation moves the Secret under an unchanged
// name. Comparing only the name misses it, and the old Secret is then stranded
// in a namespace custos has no reason to visit again.
func TestApplyTargetSecret_MovedNamespaceRemovesTheOldSecret(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	moved := renamedTo("postgres-poc-credentials", "tenant")
	require.NoError(t, m.applyTargetSecret(ctx, moved, "default", true, credentials()))

	require.NoError(t, c.Get(ctx, targetKey("default"), &corev1.Secret{}))

	err := c.Get(ctx, targetKey("tenant"), &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err),
		"the name did not change but the namespace did, so the old Secret still has to go")
}

func TestCleanupTargetSecret_DeletesItsOwnSecret(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()
	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, credentials()))

	a := arcanumWithTarget(1)
	a.Status.SecretName = "postgres-poc-credentials"
	a.Status.SecretNamespace = "tenant"
	require.NoError(t, m.cleanupTargetSecret(ctx, a))

	err := c.Get(ctx, targetKey("tenant"), &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err))
}

// If custos deleted a Secret it did not create, a name collision would turn
// into data loss in a tenant's namespace.
func TestCleanupTargetSecret_LeavesAForeignSecretAlone(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-credentials", Namespace: "tenant"},
		Data:       map[string][]byte{"KEEP": []byte("me")},
	}
	m, c := newManager(arcanumWithTarget(1), existing)
	ctx := context.Background()

	a := arcanumWithTarget(1)
	a.Status.SecretName = "postgres-poc-credentials"
	a.Status.SecretNamespace = "tenant"
	require.NoError(t, m.cleanupTargetSecret(ctx, a), "cleanup must not fail on a foreign Secret")

	require.NoError(t, c.Get(ctx, targetKey("tenant"), &corev1.Secret{}))
}

// The finalizer must release rather than block. A Secret that is already gone
// is the outcome cleanup wanted.
func TestCleanupTargetSecret_MissingSecretIsNotAnError(t *testing.T) {
	m, _ := newManager(arcanumWithTarget(1))

	a := arcanumWithTarget(1)
	a.Status.SecretName = "postgres-poc-credentials"
	a.Status.SecretNamespace = "tenant"

	require.NoError(t, m.cleanupTargetSecret(context.Background(), a))
}

// An Arcanum that never got as far as writing a Secret has nothing to clean
// up, and must not wedge on the attempt.
func TestCleanupTargetSecret_EmptyStatusIsNotAnError(t *testing.T) {
	m, _ := newManager(arcanumWithTarget(1))

	require.NoError(t, m.cleanupTargetSecret(context.Background(), arcanumWithTarget(1)))
}

// A credentials Secret is usually loaded as environment variables, so the keys
// are uppercased however the Arcanum spelled them.
func TestApplyTargetSecret_UppercasesTheKeys(t *testing.T) {
	m, c := newManager(arcanumWithTarget(1))
	ctx := context.Background()

	data := map[string]string{"host": "postgres-poc-rw", "db_name": "poc", "URL": "postgres://x"}

	require.NoError(t, m.applyTargetSecret(ctx, arcanumWithTarget(1), "tenant", false, data))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))

	assert.Equal(t, map[string][]byte{
		"HOST":    []byte("postgres-poc-rw"),
		"DB_NAME": []byte("poc"),
		"URL":     []byte("postgres://x"),
	}, got.Data, "an already uppercase key is left exactly as it is")
}
