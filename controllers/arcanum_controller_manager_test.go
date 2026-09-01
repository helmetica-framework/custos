package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
	arcanaac "github.com/helmetica-framework/custos/applyconfiguration"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(arcanav1.AddToScheme(scheme))
	return scheme
}

func arcanum(generation int64) *arcanav1.Arcanum {
	return &arcanav1.Arcanum{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sample",
			Namespace:  "default",
			Generation: generation,
			UID:        arcanumUID,
		},
		Spec: arcanav1.ArcanumSpec{},
	}
}

// arcanumUID stands in for the UID the API server would assign. Anything custos
// grows to own will copy it into an owner reference, and an owner reference
// without one is rejected in a real cluster.
const arcanumUID = types.UID("11111111-2222-3333-4444-555555555555")

func arcanumKey() types.NamespacedName {
	return types.NamespacedName{Name: "sample", Namespace: "default"}
}

// newManager wires a ArcanumManager over a fake client seeded with objs. The
// status subresource must be registered or every status write is rejected,
// and the generated type converter is what lets the status apply merge.
func newManager(objs ...client.Object) (*ArcanumManager, client.Client) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&arcanav1.Arcanum{}, &batchv1.Job{}).
		WithTypeConverters(
			arcanaac.NewTypeConverter(scheme),
			managedfields.NewDeducedTypeConverter(),
		).
		// Apply is the write path under test, so the field manager it records
		// has to survive the read back.
		WithReturnManagedFields().
		WithObjects(objs...).
		Build()

	// APIReader is the same fake client here. In production it is the
	// manager's uncached reader, so anything relying on the two differing
	// will pass in a test and fail in a cluster.
	return &ArcanumManager{
		Client:      c,
		APIReader:   c,
		Scheme:      scheme,
		Log:         logr.Discard(),
		GatherImage: "ghcr.io/helmetica-framework/custos:test",
	}, c
}

func TestReconcile_ArcanumGoneIsNoError(t *testing.T) {
	m, _ := newManager()

	res, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcile_SetsReady(t *testing.T) {
	m, c := newManager(arcanum(1), instanceNamespace(nil))

	_, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(context.Background(), arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase)
	assert.Equal(t, int64(1), got.Status.ObservedGeneration)
	assert.Equal(t, credentialsAppliedMessage, got.Status.Message)
}

func TestReconcile_IsIdempotent(t *testing.T) {
	m, c := newManager(arcanum(1), instanceNamespace(nil))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	afterFirst := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), afterFirst))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	afterSecond := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), afterSecond))

	assert.Equal(t, afterFirst.ResourceVersion, afterSecond.ResourceVersion,
		"a settled arcanum must not be written again")
}

func TestReconcile_SpecChangeRestampsObservedGeneration(t *testing.T) {
	settled := arcanum(1)
	settled.Status = arcanav1.ArcanumStatus{
		Phase:              arcanav1.ArcanumPhaseReady,
		ObservedGeneration: 1,
		Message:            credentialsAppliedMessage,
	}
	m, c := newManager(settled, instanceNamespace(nil))
	ctx := context.Background()

	// Simulate a spec edit: the API server bumps generation, status lags.
	current := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), current))
	current.Generation = 2
	current.Spec.Suspend = ptr.To(false)
	require.NoError(t, c.Update(ctx, current))

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, int64(2), got.Status.ObservedGeneration,
		"observedGeneration must follow the spec generation")
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase)
}

// TestReconcile_PhaseChangeAloneTriggersWrite pins the phase term of the
// status-write guard. The generation term and the message term both match
// here, so only a divergent Phase can make Reconcile write. Without the phase
// term the guard would treat this arcanum as already settled and skip the write,
// leaving it stuck on ArcanumPhaseFailed.
func TestReconcile_PhaseChangeAloneTriggersWrite(t *testing.T) {
	settled := arcanum(1)
	settled.Status = arcanav1.ArcanumStatus{
		Phase:              arcanav1.ArcanumPhaseFailed,
		ObservedGeneration: 1,
		Message:            credentialsAppliedMessage,
	}
	m, c := newManager(settled, instanceNamespace(nil))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase,
		"a phase mismatch alone must trigger a status write")
}

func TestReconcile_DeletingArcanumIsNotReconciledForward(t *testing.T) {
	deleting := fullArcanum(1, mappingWithGather())
	// The fake client drops an object with a deletion timestamp and no
	// finalizer, so the fixture carries a dummy one custos does not manage.
	deleting.Finalizers = []string{"test.custos/keep-alive"}
	m, c := newManager(deleting, instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	require.NoError(t, c.Delete(ctx, deleting))

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Empty(t, got.Status.Phase, "a deleting arcanum must not be driven forward")

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	assert.Empty(t, jobs.Items)
}

func TestReconcile_SuspendedArcanumHoldsAtPending(t *testing.T) {
	w := arcanum(1)
	w.Spec.Suspend = ptr.To(true)
	m, c := newManager(w, instanceNamespace(nil))

	_, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(context.Background(), arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhasePending, got.Status.Phase,
		"a suspended arcanum must not reach Ready")
	assert.Equal(t, suspendedMessage, got.Status.Message)
	assert.Equal(t, int64(1), got.Status.ObservedGeneration,
		"a suspended arcanum still records the generation it settled on")
}

func TestReconcile_SuspendFalseBehavesLikeUnset(t *testing.T) {
	w := arcanum(1)
	w.Spec.Suspend = ptr.To(false)
	m, c := newManager(w, instanceNamespace(nil))

	_, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(context.Background(), arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase,
		"an explicit false must behave exactly like an unset field")
}

func TestReconcile_UnsuspendingReturnsToReady(t *testing.T) {
	w := arcanum(1)
	w.Spec.Suspend = ptr.To(true)
	m, c := newManager(w, instanceNamespace(nil))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	current := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), current))
	current.Generation = 2
	current.Spec.Suspend = nil
	require.NoError(t, c.Update(ctx, current))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase,
		"clearing suspend must release the arcanum from Pending")
	assert.Equal(t, credentialsAppliedMessage, got.Status.Message,
		"the suspended message must be cleared, not left behind")
}

// TestReconcile_StatusIsAppliedByCustos pins the write path itself: the status
// must arrive via server-side apply under the "custos" field manager. A plain
// update writes under a different manager with a different operation, so this
// fails for anything but Apply.
func TestReconcile_StatusIsAppliedByCustos(t *testing.T) {
	m, c := newManager(arcanum(1), instanceNamespace(nil))

	_, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(context.Background(), arcanumKey(), got))

	var managers []string
	for _, mf := range got.GetManagedFields() {
		if mf.Operation == metav1.ManagedFieldsOperationApply {
			managers = append(managers, mf.Manager)
		}
	}
	assert.Contains(t, managers, "custos",
		"the status must be written by an apply from the custos field manager")
}

func mappingWithoutGather() arcanav1.CredentialsSpec {
	return arcanav1.CredentialsSpec{ValueMapping: map[string]arcanav1.ValueSource{
		"HOST": {Source: arcanav1.SourceConst, Value: "postgres-poc-rw"},
		"URL":  {Source: arcanav1.SourceTemplate, Value: "postgres://{{.HOST}}:5432"},
	}}
}

func mappingWithGather() arcanav1.CredentialsSpec {
	return arcanav1.CredentialsSpec{ValueMapping: map[string]arcanav1.ValueSource{
		"HOST": {Source: arcanav1.SourceConst, Value: "postgres-poc-rw"},
		"USERNAME": {
			Source: arcanav1.SourceObjectRef, APIVersion: "v1", Kind: "Secret",
			Name: "{{.ClaimName}}-app", Key: "username",
		},
	}}
}

func fullArcanum(generation int64, creds arcanav1.CredentialsSpec) *arcanav1.Arcanum {
	a := arcanum(generation)
	a.Spec.Target = arcanav1.TargetSpec{Name: "postgres-poc-credentials"}
	a.Spec.Credentials = creds
	return a
}

// A mapping custos can resolve alone must settle in one pass. Spending a Job
// on it would make every simple Arcanum asynchronous for nothing.
func TestReconcile_NoGatherNeededSettlesInOnePass(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithoutGather()), instanceNamespace(nil))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	assert.Empty(t, jobs.Items, "no gather was needed, so no Job may exist")

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("default"), secret))

	// StringData, not Data. A real API server moves the one into the other on
	// write; the fake client stores what was sent.
	assert.Equal(t, "postgres://postgres-poc-rw:5432", secret.StringData["URL"])
}

// Without the annotation the Secret stays put, which is the plain helm case.
func TestReconcile_WithoutAClaimTheSecretIsLocal(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithoutGather()), instanceNamespace(nil))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, "default", got.Status.SecretNamespace)
	assert.Empty(t, got.Finalizers, "a local target is handled by an owner reference, not a finalizer")
}

func TestReconcile_WithAClaimTheSecretGoesToTheClaimNamespace(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithoutGather()), instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	require.NoError(t, c.Get(ctx, targetKey("tenant"), &corev1.Secret{}))

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, "tenant", got.Status.SecretNamespace)
	assert.Contains(t, got.Finalizers, targetFinalizer,
		"a cross-namespace Secret cannot be garbage collected, so it needs a finalizer")
}

func TestReconcile_GatherNeededCreatesTheJobAndHoldsAtPending(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithGather()), instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	require.Len(t, jobs.Items, 1)

	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "sample-gather", Namespace: "default",
	}, cm), "the Job reads its plan from this ConfigMap")

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhasePending, got.Status.Phase)
	assert.Equal(t, jobs.Items[0].Name, got.Status.GatherJobName)
}

func TestReconcile_SucceededGatherProducesTheSecret(t *testing.T) {
	a := fullArcanum(1, mappingWithGather())
	m, c := newManager(a, instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	// First pass creates the Job.
	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	require.Len(t, jobs.Items, 1)

	// Stand in for the Job: mark it succeeded and drop the gathered Secret.
	job := jobs.Items[0]
	job.Status.Succeeded = 1
	require.NoError(t, c.Status().Update(ctx, &job))
	require.NoError(t, c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-gathered", Namespace: "default"},
		Data:       map[string][]byte{"USERNAME": []byte("app")},
	}))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, targetKey("tenant"), secret))
	assert.Equal(t, "app", secret.StringData["USERNAME"])
	assert.Equal(t, "postgres-poc-rw", secret.StringData["HOST"])

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase)
}

// The result ConfigMap is why custos does not need pods/log, so the message
// has to actually come from it.
func TestReconcile_FailedGatherSurfacesTheResultMessage(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithGather()), instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	job.Status.Failed = 1
	require.NoError(t, c.Status().Update(ctx, &job))
	require.NoError(t, c.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-gather-result", Namespace: "default"},
		Data: map[string]string{
			"status":  "error",
			"message": `objectRef USERNAME: secrets "postgres-poc-app" not found`,
		},
	}))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "postgres-poc-app")
}

// A changed plan means a new Job name, since a Job's spec is immutable. The
// Job for the old hash has to go, or every spec edit leaves another one
// behind until the namespace is full of them.
func TestReconcile_ChangedMappingRemovesTheStaleGatherJob(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithGather()), instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	require.Len(t, jobs.Items, 1)
	firstJob := jobs.Items[0].Name

	current := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), current))
	current.Generation = 2
	current.Spec.Credentials.ValueMapping["USERNAME"] = arcanav1.ValueSource{
		Source: arcanav1.SourceObjectRef, APIVersion: "v1", Kind: "Secret",
		Name: "{{.ClaimName}}-app", Key: "user",
	}
	require.NoError(t, c.Update(ctx, current))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	require.NoError(t, c.List(ctx, jobs))
	require.Len(t, jobs.Items, 1, "the Job for the previous hash must be gone")
	assert.NotEqual(t, firstJob, jobs.Items[0].Name)
}

// Validation runs before anything is created, because an exec is allowed to
// provision and a spec that can never render must not get that far.
func TestReconcile_InvalidMappingFailsBeforeCreatingAJob(t *testing.T) {
	bad := fullArcanum(1, arcanav1.CredentialsSpec{
		ValueMapping: map[string]arcanav1.ValueSource{
			"USERNAME": {
				Source: arcanav1.SourceObjectRef, APIVersion: "v1", Kind: "Secret",
				Name: "pg-app", Key: "username",
			},
			"URL": {Source: arcanav1.SourceTemplate, Value: "{{.NOPE}}"},
		},
	})
	m, c := newManager(bad, instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	assert.Empty(t, jobs.Items, "an invalid mapping must never reach the gather stage")

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "NOPE")
}

// Suspend is the escape hatch for when custos and the cluster disagree. It
// must not pull a working service's credentials away.
func TestReconcile_SuspendedArcanumCreatesNoJobAndKeepsItsSecret(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithoutGather()), instanceNamespace(nil))
	ctx := context.Background()
	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	current := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), current))
	current.Generation = 2
	current.Spec.Suspend = ptr.To(true)
	current.Spec.Credentials = mappingWithGather()
	require.NoError(t, c.Update(ctx, current))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(ctx, jobs))
	assert.Empty(t, jobs.Items)

	require.NoError(t, c.Get(ctx, targetKey("default"), &corev1.Secret{}),
		"suspending must leave an already written Secret in place")
}

// The finalizer path: deleting the Arcanum takes the remote Secret with it.
func TestReconcile_DeletionRemovesTheRemoteSecretAndTheFinalizer(t *testing.T) {
	m, c := newManager(fullArcanum(1, mappingWithoutGather()), instanceNamespace(chrysoAnnotations()))
	ctx := context.Background()
	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, targetKey("tenant"), &corev1.Secret{}))

	current := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), current))
	require.NoError(t, c.Delete(ctx, current))

	_, err = m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	err = c.Get(ctx, targetKey("tenant"), &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "the credentials must go with the Arcanum")

	err = c.Get(ctx, arcanumKey(), &arcanav1.Arcanum{})
	assert.True(t, apierrors.IsNotFound(err),
		"removing the last finalizer lets the API server complete the delete")
}
