package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		WithStatusSubresource(&arcanav1.Arcanum{}).
		WithTypeConverters(
			arcanaac.NewTypeConverter(scheme),
			managedfields.NewDeducedTypeConverter(),
		).
		// Apply is the write path under test, so the field manager it records
		// has to survive the read back.
		WithReturnManagedFields().
		WithObjects(objs...).
		Build()

	return &ArcanumManager{
		Client: c,
		Scheme: scheme,
		Log:    logr.Discard(),
	}, c
}

func TestReconcile_ArcanumGoneIsNoError(t *testing.T) {
	m, _ := newManager()

	res, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcile_SetsReady(t *testing.T) {
	m, c := newManager(arcanum(1))

	_, err := m.Reconcile(context.Background(), ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(context.Background(), arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase)
	assert.Equal(t, int64(1), got.Status.ObservedGeneration)
	assert.Equal(t, readyMessage, got.Status.Message)
}

func TestReconcile_IsIdempotent(t *testing.T) {
	m, c := newManager(arcanum(1))
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
		Message:            readyMessage,
	}
	m, c := newManager(settled)
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
		Message:            readyMessage,
	}
	m, c := newManager(settled)
	ctx := context.Background()

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Equal(t, arcanav1.ArcanumPhaseReady, got.Status.Phase,
		"a phase mismatch alone must trigger a status write")
}

func TestReconcile_DeletingArcanumIsSkipped(t *testing.T) {
	deleting := arcanum(1)
	// The fake client drops an object that has a deletion timestamp and no
	// finalizer, so the fixture carries a dummy one. The controller does not
	// manage this finalizer.
	deleting.Finalizers = []string{"test.custos/keep-alive"}
	m, c := newManager(deleting)
	ctx := context.Background()

	require.NoError(t, c.Delete(ctx, deleting))

	_, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: arcanumKey()})
	require.NoError(t, err)

	got := &arcanav1.Arcanum{}
	require.NoError(t, c.Get(ctx, arcanumKey(), got))
	assert.Empty(t, got.Status.Phase, "a arcanum being deleted must not be touched")
}

func TestReconcile_SuspendedArcanumHoldsAtPending(t *testing.T) {
	w := arcanum(1)
	w.Spec.Suspend = ptr.To(true)
	m, c := newManager(w)

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
	m, c := newManager(w)

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
	m, c := newManager(w)
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
	assert.Equal(t, readyMessage, got.Status.Message,
		"the suspended message must be cleared, not left behind")
}

// TestReconcile_StatusIsAppliedByCustos pins the write path itself: the status
// must arrive via server-side apply under the "custos" field manager. A plain
// update writes under a different manager with a different operation, so this
// fails for anything but Apply.
func TestReconcile_StatusIsAppliedByCustos(t *testing.T) {
	m, c := newManager(arcanum(1))

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
