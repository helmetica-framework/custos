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

	custosv1 "github.com/helmetica-framework/custos/api/v1"
)

func credentials() map[string]string {
	return map[string]string{"HOST": "postgres-poc-rw", "PASSWORD": "hunter2"}
}

// targetKey is where the Secret lands: "tenant" is the claim namespace of
// chrysoAnnotations, anything else is the plain helm case.
func targetKey(ns string) types.NamespacedName {
	md := metadata{ClaimName: "postgres-poc", ClaimKind: "PostgreSQL", ClaimAPIVersion: "v6.postgres.helmetica-bundles.io/v1"}

	name, err := targetSecretName(credentialConfig(1), md, ns == "tenant")
	if err != nil {
		panic(err)
	}

	return types.NamespacedName{Name: name, Namespace: ns}
}

func secretKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: ns}
}

// renamedTo is an CredentialConfig whose target has been pointed at a new name, with
// the status still recording where the last pass wrote.
func renamedTo(oldNamespace string) *custosv1.CredentialConfig {
	a := credentialConfig(1)
	a.Status.SecretName = targetKey(oldNamespace).Name
	a.Status.SecretNamespace = oldNamespace

	return a
}

func TestApplyTargetSecret_WritesTheData(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

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
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Equal(t, "sample", got.Labels[credentialConfigNameLabel])
	assert.Equal(t, "default", got.Labels[credentialConfigNamespaceLabel])
	assert.Equal(t, string(credentialConfigUID), got.Labels[credentialConfigUIDLabel])
}

// Owner references cannot cross namespaces, so only the local case gets one.
func TestApplyTargetSecret_LocalTargetGetsAnOwnerReference(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "default", targetKey("default").Name, true, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("default"), got))
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, credentialConfigUID, got.OwnerReferences[0].UID)
}

func TestApplyTargetSecret_RemoteTargetGetsNoOwnerReference(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Empty(t, got.OwnerReferences,
		"an owner reference across namespaces would make the garbage collector delete the Secret")
}

// This is the guard that stops an CredentialConfig from taking over a Secret the
// tenant already had at that name.
func TestApplyTargetSecret_RefusesAForeignSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: targetKey("tenant").Name, Namespace: "tenant"},
		Data:       map[string][]byte{"KEEP": []byte("me")},
	}
	m, c := newManager(credentialConfig(1), existing)
	ctx := context.Background()

	err := m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials())

	require.Error(t, err)
	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Equal(t, map[string][]byte{"KEEP": []byte("me")}, got.Data,
		"a Secret custos did not create must be left exactly as it was")
}

func TestApplyTargetSecret_UpdatesItsOwnSecret(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()
	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	rotated := map[string]string{"HOST": "postgres-poc-rw", "PASSWORD": "hunter3"}
	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, rotated))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))
	assert.Equal(t, []byte("hunter3"), got.Data["PASSWORD"])
}

// Changing spec.target.prefix has to take the old Secret with it. Leaving it
// behind would keep serving credentials from a name nothing points at any
// more, and no later pass would find it: the status has moved on to the new
// name, which is the only handle cleanup has.
func TestApplyTargetSecret_RenameRemovesTheOldSecret(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	renamed := renamedTo("tenant")
	require.NoError(t, m.applyTargetSecret(ctx, renamed, "tenant", "postgres-poc-renamed", false, credentials()))

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
	m, c := newManager(credentialConfig(1), foreign)
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	renamed := renamedTo("tenant")

	err := m.applyTargetSecret(ctx, renamed, "tenant", "postgres-poc-renamed", false, credentials())

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
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	moved := renamedTo("tenant")
	require.NoError(t, m.applyTargetSecret(ctx, moved, "default", targetKey("tenant").Name, true, credentials()))

	require.NoError(t, c.Get(ctx, secretKey("default", targetKey("tenant").Name), &corev1.Secret{}))

	err := c.Get(ctx, targetKey("tenant"), &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err),
		"the name did not change but the namespace did, so the old Secret still has to go")
}

func TestCleanupTargetSecret_DeletesItsOwnSecret(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()
	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, credentials()))

	a := credentialConfig(1)
	a.Status.SecretName = targetKey("tenant").Name
	a.Status.SecretNamespace = "tenant"
	require.NoError(t, m.cleanupTargetSecret(ctx, a))

	err := c.Get(ctx, targetKey("tenant"), &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err))
}

// If custos deleted a Secret it did not create, a name collision would turn
// into data loss in a tenant's namespace.
func TestCleanupTargetSecret_LeavesAForeignSecretAlone(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: targetKey("tenant").Name, Namespace: "tenant"},
		Data:       map[string][]byte{"KEEP": []byte("me")},
	}
	m, c := newManager(credentialConfig(1), existing)
	ctx := context.Background()

	a := credentialConfig(1)
	a.Status.SecretName = targetKey("tenant").Name
	a.Status.SecretNamespace = "tenant"
	require.NoError(t, m.cleanupTargetSecret(ctx, a), "cleanup must not fail on a foreign Secret")

	require.NoError(t, c.Get(ctx, targetKey("tenant"), &corev1.Secret{}))
}

// The finalizer must release rather than block. A Secret that is already gone
// is the outcome cleanup wanted.
func TestCleanupTargetSecret_MissingSecretIsNotAnError(t *testing.T) {
	m, _ := newManager(credentialConfig(1))

	a := credentialConfig(1)
	a.Status.SecretName = targetKey("tenant").Name
	a.Status.SecretNamespace = "tenant"

	require.NoError(t, m.cleanupTargetSecret(context.Background(), a))
}

// An CredentialConfig that never got as far as writing a Secret has nothing to clean
// up, and must not wedge on the attempt.
func TestCleanupTargetSecret_EmptyStatusIsNotAnError(t *testing.T) {
	m, _ := newManager(credentialConfig(1))

	require.NoError(t, m.cleanupTargetSecret(context.Background(), credentialConfig(1)))
}

// A credentials Secret is usually loaded as environment variables, so the keys
// are uppercased however the CredentialConfig spelled them.
func TestApplyTargetSecret_UppercasesTheKeys(t *testing.T) {
	m, c := newManager(credentialConfig(1))
	ctx := context.Background()

	data := map[string]string{"host": "postgres-poc-rw", "db_name": "poc", "URL": "postgres://x"}

	require.NoError(t, m.applyTargetSecret(ctx, credentialConfig(1), "tenant", targetKey("tenant").Name, false, data))

	got := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), got))

	assert.Equal(t, map[string][]byte{
		"HOST":    []byte("postgres-poc-rw"),
		"DB_NAME": []byte("poc"),
		"URL":     []byte("postgres://x"),
	}, got.Data, "an already uppercase key is left exactly as it is")
}

func TestTargetSecretName_PrefixPlusClaimKindHash(t *testing.T) {
	redis := metadata{ClaimName: "example", ClaimKind: "Redis", ClaimAPIVersion: "v0.redis.helmetica-bundles.io/v1"}
	postgres := metadata{ClaimName: "example", ClaimKind: "PostgreSQL", ClaimAPIVersion: "v6.postgres.helmetica-bundles.io/v1"}
	redisV2 := metadata{ClaimName: "example", ClaimKind: "Redis", ClaimAPIVersion: "v0.redis.helmetica-bundles.io/v2"}

	name := func(a *custosv1.CredentialConfig, md metadata, hasClaim bool) string {
		n, err := targetSecretName(a, md, hasClaim)
		require.NoError(t, err)
		return n
	}

	plain := credentialConfig(1)
	prefixed := credentialConfig(1)
	prefixed.Spec.Target.Prefix = "app"

	assert.Regexp(t, `^example-[0-9a-f]{8}$`, name(plain, redis, true))
	assert.NotEqual(t, name(plain, redis, true), name(plain, postgres, true), "same claim name, different kind")
	assert.Equal(t, name(plain, redis, true), name(plain, redisV2, true), "version bump must not rename the secret")
	assert.Regexp(t, `^app-[0-9a-f]{8}$`, name(prefixed, redis, true))
	assert.NotEqual(t, name(prefixed, redis, true), name(prefixed, postgres, true), "a prefix must not defeat the hash")
	assert.Regexp(t, `^sample-[0-9a-f]{8}$`, name(plain, metadata{}, false))
	assert.Regexp(t, `^app-[0-9a-f]{8}$`, name(prefixed, metadata{}, false), "a prefix is never the whole name")
}
