package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
	arcanaacv1 "github.com/helmetica-framework/custos/applyconfiguration/api/v1"
)

const (
	fieldOwner = client.FieldOwner("custos")

	readyMessage     = "nothing to guard yet"
	suspendedMessage = "reconciliation suspended"
)

// ArcanumManager reconciles Arcanum objects.
type ArcanumManager struct {
	client.Client
	// APIReader bypasses the cache. Reads that must not be served stale, and
	// reads of kinds custos has no business starting an informer for, go
	// through it rather than through Client.
	APIReader client.Reader
	// GatherImage is the custos image the gather Job runs. It is the same
	// binary as the controller, invoked as `custos gather`.
	GatherImage string
	Scheme      *runtime.Scheme
	Recorder    events.EventRecorder
	Log         logr.Logger
}

type phase struct {
	Phase   arcanav1.ArcanumPhase
	Message string
}

// +kubebuilder:rbac:groups=arcana.helmetica.io,resources=arcana,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=arcana.helmetica.io,resources=arcana/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives a Arcanum's status to reflect desiredPhase. There is nothing
// else to do yet: the placeholder owns no other cluster objects, so this is
// just a settle-and-write loop over the Arcanum itself.
func (r *ArcanumManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("arcanum", req.NamespacedName)

	arcanum := &arcanav1.Arcanum{}
	err := r.Get(ctx, req.NamespacedName, arcanum)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("arcanum is gone, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !arcanum.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("arcanum is being deleted, nothing to do")
		return ctrl.Result{}, nil
	}

	want, err := r.desiredPhase(ctx, arcanum)
	if err != nil {
		return ctrl.Result{}, err
	}

	if arcanum.Status.Phase == want.Phase &&
		arcanum.Status.ObservedGeneration == arcanum.Generation &&
		arcanum.Status.Message == want.Message {
		return ctrl.Result{}, nil
	}

	if arcanum.Status.Phase != want.Phase {
		log.Info("arcanum phase changed", "from", arcanum.Status.Phase, "to", want.Phase)
	}

	status := arcanaacv1.Arcanum(arcanum.GetName(), arcanum.GetNamespace()).
		WithStatus(arcanaacv1.ArcanumStatus().
			WithPhase(want.Phase).
			WithObservedGeneration(arcanum.Generation).
			WithMessage(want.Message))

	if err := r.Status().Apply(ctx, status, fieldOwner, client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying arcanum status: %w", err)
	}
	return ctrl.Result{}, nil
}

// desiredPhase is the seam where custos's real logic goes. Everything it will
// eventually provision belongs behind the suspend check, so that suspending a
// arcanum keeps working once there is something to suspend. The error return is
// unused today and is here for that logic to use.
func (r *ArcanumManager) desiredPhase(_ context.Context, arcanum *arcanav1.Arcanum) (phase, error) {
	if ptr.Deref(arcanum.Spec.Suspend, false) {
		return phase{Phase: arcanav1.ArcanumPhasePending, Message: suspendedMessage}, nil
	}

	return phase{Phase: arcanav1.ArcanumPhaseReady, Message: readyMessage}, nil
}

// SetupWithManager wires the controller: watch Arcanum. Nothing else is owned
// yet, so there are no additional watches.
func (r *ArcanumManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&arcanav1.Arcanum{}).
		Complete(r)
}
