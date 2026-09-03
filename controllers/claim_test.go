package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func instanceNamespace(annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Annotations: annotations},
	}
}

func chrysoAnnotations() map[string]string {
	return map[string]string{
		claimNamespaceAnnotation:  "tenant",
		claimNameAnnotation:       "postgres-poc",
		claimKindAnnotation:       "PostgreSQL",
		claimAPIVersionAnnotation: "v6.postgres.helmetica-bundles.io/v1",
	}
}

func TestInstanceContext_ReadsChrysoAnnotations(t *testing.T) {
	m, _ := newManager(arcanum(1), instanceNamespace(chrysoAnnotations()))

	md, hasClaim, err := m.instanceContext(context.Background(), arcanum(1))

	require.NoError(t, err)
	assert.True(t, hasClaim)
	assert.Equal(t, "postgres-poc", md.ClaimName)
	assert.Equal(t, "tenant", md.ClaimNamespace)
	assert.Equal(t, "PostgreSQL", md.ClaimKind)
	assert.Equal(t, "v6.postgres.helmetica-bundles.io/v1", md.ClaimAPIVersion)
	assert.Equal(t, "default", md.InstanceNamespace)
	assert.Equal(t, "sample", md.ArcanumName)
}

// A namespace with no chryso annotation is a plain helm install. That is a
// supported case, not an error: the Arcanum simply writes its Secret locally.
func TestInstanceContext_UnannotatedNamespaceHasNoClaim(t *testing.T) {
	m, _ := newManager(arcanum(1), instanceNamespace(nil))

	md, hasClaim, err := m.instanceContext(context.Background(), arcanum(1))

	require.NoError(t, err)
	assert.False(t, hasClaim)
	assert.Empty(t, md.ClaimName)
	assert.Equal(t, "default", md.InstanceNamespace)
}

// A half-stamped namespace means chryso changed under us or somebody edited
// the annotations by hand. Guessing would produce a lookup against an empty
// kind, so it fails loudly instead.
func TestInstanceContext_PartialAnnotationsAreAnError(t *testing.T) {
	partial := chrysoAnnotations()
	delete(partial, claimKindAnnotation)
	m, _ := newManager(arcanum(1), instanceNamespace(partial))

	_, _, err := m.instanceContext(context.Background(), arcanum(1))

	require.Error(t, err)
	assert.Contains(t, err.Error(), claimKindAnnotation)
}

func TestTargetNamespace_ClaimNamespaceWhenThereIsAClaim(t *testing.T) {
	md := metadata{ClaimNamespace: "tenant", InstanceNamespace: "instance"}

	assert.Equal(t, "tenant", targetNamespace(md, true))
}

func TestTargetNamespace_OwnNamespaceWhenThereIsNot(t *testing.T) {
	md := metadata{InstanceNamespace: "instance"}

	assert.Equal(t, "instance", targetNamespace(md, false))
}

// claimObject has to read through APIReader, because the claim's kind is only
// known at runtime and a cached read would start an informer per claim kind.
// newManager points Client and APIReader at one fake client, so the only way
// to tell which one was used is to give them different contents: here the
// claim exists for the uncached reader and nowhere else.
func TestClaimObject_ReadsThroughTheUncachedReader(t *testing.T) {
	scheme := newTestScheme()
	gvk := schema.GroupVersionKind{
		Group:   "v6.postgres.helmetica-bundles.io",
		Version: "v1",
		Kind:    "PostgreSQL",
	}
	// The claim kind is a CRD chrysopoeia generates at runtime, so the scheme
	// has to be told about it by hand before a fake client can serve one.
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(gvk)
	claim.SetName("postgres-poc")
	claim.SetNamespace("tenant")

	m, _ := newManager(arcanum(1))
	m.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()

	got, err := m.claimObject(context.Background(), testMetadata())

	require.NoError(t, err)
	assert.Equal(t, "postgres-poc", got.GetName())
	assert.Equal(t, "tenant", got.GetNamespace())
}
