package gather

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "x-abcd1234-tenant-postgres-poc"

func newResolver(objs ...client.Object) *Resolver {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	return &Resolver{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
	}
}

func appSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-app", Namespace: testNamespace},
		Data: map[string][]byte{
			"username": []byte("app"),
			"password": []byte("hunter2"),
		},
	}
}

// A Secret's data is base64 in the API but []byte in Go. The resolved value
// has to be the plaintext, not the encoding.
func TestResolve_SecretKeyIsDecoded(t *testing.T) {
	r := newResolver(appSecret())
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "USERNAME", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "Secret",
		Name: "postgres-poc-app", DataKey: "username",
	}}}

	got, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"USERNAME": "app"}, got)
}

func TestResolve_ConfigMapKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: testNamespace},
		Data:       map[string]string{"retention": "6"},
	}
	r := newResolver(cm)
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "RETENTION", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "ConfigMap",
		Name: "backup", DataKey: "retention",
	}}}

	got, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"RETENTION": "6"}, got)
}

// The point of generalising away from a secret reference: any object, any
// field.
func TestResolve_JSONPathIntoAnArbitraryObject(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-rw", Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	r := newResolver(svc)
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "PORT", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "Service",
		Name: "postgres-poc-rw", Path: "{.spec.ports[0].port}",
	}}}

	got, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"PORT": "5432"}, got)
}

// A partial gather must never reach the render stage, so one failure fails
// the whole run.
func TestResolve_MissingObjectFailsTheWholeRun(t *testing.T) {
	r := newResolver()
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "USERNAME", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "Secret",
		Name: "postgres-poc-app", DataKey: "username",
	}}}

	_, err := r.Resolve(context.Background(), plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "USERNAME")
	assert.Contains(t, err.Error(), "postgres-poc-app")
}

// The message lands on the Arcanum's status, which anyone with read access to
// the claim can see. It must say what failed without saying what was read.
func TestResolve_MissingDataKeyErrorDoesNotLeakTheValue(t *testing.T) {
	r := newResolver(appSecret())
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "USERNAME", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "Secret",
		Name: "postgres-poc-app", DataKey: "nope",
	}}}

	_, err := r.Resolve(context.Background(), plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
	assert.NotContains(t, err.Error(), "hunter2")
}

// A path off the CRD arrives bare, because validate only rejects braces on a
// claimParam. Both spellings have to reach the same field: handing the parser
// a bare path does not fail, it hands the path back as literal text, and that
// literal would be written into a credential.
func TestResolve_JSONPathWithoutBracesReadsTheSameField(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-rw", Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	r := newResolver(svc)
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "PORT", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "Service",
		Name: "postgres-poc-rw", Path: ".spec.ports[0].port",
	}}}

	got, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"PORT": "5432"}, got)
}

// A typo in a path must fail the run. The jsonpath default is to treat a
// missing key as an empty result, which would write an empty credential and
// report success.
func TestResolve_PathThatSelectsNothingIsAnError(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-poc-rw", Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	r := newResolver(svc)
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "PORT", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "Service",
		Name: "postgres-poc-rw", Path: "{.spec.porst[0].port}",
	}}}

	_, err := r.Resolve(context.Background(), plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PORT")
}

// A ConfigMap value is stored as written. Decoding one because it happens to
// be valid base64 would turn a retention of "6" into mojibake.
func TestResolve_ConfigMapValueThatLooksLikeBase64IsNotDecoded(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: testNamespace},
		Data:       map[string]string{"bucket": "aGVsbG8="},
	}
	r := newResolver(cm)
	plan := Plan{Namespace: testNamespace, Entries: []Entry{{
		Key: "BUCKET", Kind: EntryObjectRef,
		APIVersion: "v1", ObjectKind: "ConfigMap",
		Name: "backup", DataKey: "bucket",
	}}}

	got, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"BUCKET": "aGVsbG8="}, got)
}
