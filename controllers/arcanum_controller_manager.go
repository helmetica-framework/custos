package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
	arcanaacv1 "github.com/helmetica-framework/custos/applyconfiguration/api/v1"
)

const (
	credentialsAppliedMessage = "credentials applied"
	gatheringMessage          = "gathering values"
	suspendedMessage          = "reconciliation suspended"
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

// phase is everything one pass decided. It carries the recorded fields as
// well as the phase itself, because the status write is a single apply and a
// second return value per field would be four more.
type phase struct {
	Phase           arcanav1.ArcanumPhase
	Message         string
	GatheredHash    string
	GatherJobName   string
	SecretName      string
	SecretNamespace string
	// RequeueAfter is when to look again when nothing else will tell us. A Job
	// that has stopped changing produces no more watch events, so a retry that
	// is not scheduled here never happens.
	RequeueAfter time.Duration
}

// +kubebuilder:rbac:groups=arcana.helmetica.io,resources=arcana,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=arcana.helmetica.io,resources=arcana/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arcana.helmetica.io,resources=arcana/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete

// Reconcile drives an Arcanum's status to reflect desiredPhase, and takes the
// target Secret back when the Arcanum goes away.
//
// The deletion branch runs before anything else. A Secret in the claim
// namespace cannot carry an owner reference, so the finalizer is the only
// thing that removes it.
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
		log.V(1).Info("arcanum is being deleted, cleaning finalizer")

		if controllerutil.ContainsFinalizer(arcanum, targetFinalizer) {
			err := r.cleanupTargetSecret(ctx, arcanum)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("removing target secret: %w", err)
			}
		}

		patch := client.MergeFrom(arcanum.DeepCopy())
		controllerutil.RemoveFinalizer(arcanum, targetFinalizer)

		return ctrl.Result{}, r.Patch(ctx, arcanum, patch)
	}

	want, err := r.desiredPhase(ctx, arcanum)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A retry lives here rather than in the status, so both exits below have to
	// carry it. Dropping it on the settled path leaves the Arcanum at Failed
	// with nothing left to wake it.
	result := ctrl.Result{RequeueAfter: want.RequeueAfter}

	// Every recorded field is compared, or a decision that only moved the Job
	// name would be made every pass and written none of them.
	if arcanum.Status.Phase == want.Phase &&
		arcanum.Status.ObservedGeneration == arcanum.Generation &&
		arcanum.Status.Message == want.Message &&
		arcanum.Status.GatheredHash == want.GatheredHash &&
		arcanum.Status.GatherJobName == want.GatherJobName &&
		arcanum.Status.SecretName == want.SecretName &&
		arcanum.Status.SecretNamespace == want.SecretNamespace {
		return result, nil
	}

	if arcanum.Status.Phase != want.Phase {
		log.Info("arcanum phase changed", "from", arcanum.Status.Phase, "to", want.Phase)
	}

	status := arcanaacv1.Arcanum(arcanum.GetName(), arcanum.GetNamespace()).
		WithStatus(arcanaacv1.ArcanumStatus().
			WithPhase(want.Phase).
			WithObservedGeneration(arcanum.Generation).
			WithMessage(want.Message).
			WithGatheredHash(want.GatheredHash).
			WithGatherJobName(want.GatherJobName).
			WithSecretName(want.SecretName).
			WithSecretNamespace(want.SecretNamespace))

	if err := r.Status().Apply(ctx, status, fieldOwner, client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying arcanum status: %w", err)
	}

	return result, nil
}

// desiredPhase runs one pass of the whole flow and reports what the status
// should say. Everything it provisions sits behind the suspend check, so
// suspending an Arcanum leaves a Secret it already wrote in place.
//
// Most failures here are the phase rather than a returned error: a mapping
// that cannot render, a claim that is not there and a target name already
// taken are all things a retry cannot fix, and returning an error would have
// the queue back off over them forever.
func (r *ArcanumManager) desiredPhase(ctx context.Context, arcanum *arcanav1.Arcanum) (phase, error) {
	// Carried forward, because a pass that holds or fails must not wipe what
	// an earlier one recorded. cleanupTargetSecret finds the Secret through
	// status.SecretName, so losing that would strand it.
	want := phase{
		GatheredHash:    arcanum.Status.GatheredHash,
		GatherJobName:   arcanum.Status.GatherJobName,
		SecretName:      arcanum.Status.SecretName,
		SecretNamespace: arcanum.Status.SecretNamespace,
	}

	if ptr.Deref(arcanum.Spec.Suspend, false) {
		want.Phase = arcanav1.ArcanumPhasePending
		want.Message = suspendedMessage

		return want, nil
	}

	md, hasClaim, err := r.instanceContext(ctx, arcanum)
	if err != nil {
		return failedPhase(want, err), nil
	}

	mapping := arcanum.Spec.Credentials.ValueMapping

	if err := validate(mapping, hasClaim); err != nil {
		return failedPhase(want, err), nil
	}

	// An Arcanum with no mapping is legal and has nothing to write. A Secret
	// with no keys would only be something for a consumer to trip over.
	if len(mapping) == 0 {
		want.Phase = arcanav1.ArcanumPhaseReady
		want.Message = credentialsAppliedMessage

		return want, nil
	}

	// Before the write, not after. A delete landing in between would
	// otherwise leave the Secret behind in a namespace custos no longer has
	// any reason to visit.
	if hasClaim {
		if err := r.ensureTargetFinalizer(ctx, arcanum); err != nil {
			return want, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	var gathered map[string]string

	if needsGather(mapping) {
		values, hold, err := r.gatherValues(ctx, arcanum, md, mapping, &want)
		if err != nil || hold {
			return want, err
		}

		gathered = values
	}

	claim, err := r.claimFor(ctx, md, mapping)
	if err != nil {
		return failedPhase(want, err), nil
	}

	rendered, err := render(mapping, md, gathered, claim)
	if err != nil {
		return failedPhase(want, err), nil
	}

	ns := targetNamespace(md, hasClaim)

	if err := r.applyTargetSecret(ctx, arcanum, ns, !hasClaim, rendered); err != nil {
		return failedPhase(want, err), nil
	}

	want.Phase = arcanav1.ArcanumPhaseReady
	want.Message = credentialsAppliedMessage
	want.SecretName = arcanum.Spec.Target.Name
	want.SecretNamespace = ns

	return want, nil
}

// gatherValues runs the gather stage. It returns hold when the pass has to
// stop where it is, either waiting on a Job or reporting a failed one, having
// already written what to say into want.
func (r *ArcanumManager) gatherValues(
	ctx context.Context,
	arcanum *arcanav1.Arcanum,
	md metadata,
	mapping map[string]arcanav1.ValueSource,
	want *phase,
) (values map[string]string, hold bool, err error) {
	plan, err := buildPlan(mapping, md)
	if err != nil {
		*want = failedPhase(*want, err)

		return nil, true, nil
	}

	// The refresh annotation is hashed in, so changing it is what makes this
	// a different Job and re-runs a gather that has already settled.
	hash := gatherHash(plan, arcanum.GetAnnotations()[refreshAnnotation])
	jobName := gatherJobName(arcanum.GetName(), hash)

	// Pruned before anything is decided, so a changed plan does not leave the
	// Job for the old one running alongside the new one.
	if err := r.deleteStaleGatherJobs(ctx, arcanum, jobName); err != nil {
		return nil, true, err
	}

	gathered, found, err := r.gatheredValues(ctx, arcanum)
	if err != nil {
		return nil, true, err
	}

	// The values in hand came from this very plan, so there is nothing to run.
	if found && arcanum.Status.GatheredHash == hash {
		return gathered, false, nil
	}

	want.GatherJobName = jobName

	job := &batchv1.Job{}

	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: arcanum.GetNamespace()}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, true, fmt.Errorf("getting gather job: %w", err)
	}

	if apierrors.IsNotFound(err) {
		if err := r.startGather(ctx, arcanum, plan, hash); err != nil {
			return nil, true, err
		}

		want.Phase = arcanav1.ArcanumPhasePending
		want.Message = gatheringMessage

		return nil, true, nil
	}

	// Above the switch, because a Job that gave up also has failed pods and
	// would otherwise be read as one that is still trying.
	if jobGaveUp(job) {
		want.Phase = arcanav1.ArcanumPhaseFailed
		want.Message = r.gatherResultMessage(ctx, arcanum,
			fmt.Sprintf("gather job %s failed and left no result, see its logs", jobName))

		return nil, true, r.retryGather(ctx, job, want)
	}

	switch {
	case job.Status.Succeeded > 0 && !found:
		// A Job that finished without leaving values either had its Secret
		// removed, or exited zero on a failure. It reports what went wrong
		// either way, and that beats rendering an empty credential.
		want.Phase = arcanav1.ArcanumPhaseFailed
		want.Message = r.gatherResultMessage(ctx, arcanum,
			fmt.Sprintf("gather job %s succeeded but left no values", jobName))

		return nil, true, r.retryGather(ctx, job, want)

	case job.Status.Succeeded > 0:
		want.GatheredHash = hash

		return gathered, false, nil

	case job.Status.Failed > 0:
		// An attempt failed and another is coming. The reason on a Pending is
		// more use than Failed at something that is about to fix itself.
		want.Phase = arcanav1.ArcanumPhasePending
		want.Message = r.gatherResultMessage(ctx, arcanum, gatheringMessage)

		return nil, true, nil

	default:
		want.Phase = arcanav1.ArcanumPhasePending
		want.Message = gatheringMessage

		return nil, true, nil
	}
}

// claimFor reads the claim only when the mapping actually needs it, so an
// Arcanum that never mentions the claim does not fail because that one object
// happens to be unreadable.
func (r *ArcanumManager) claimFor(
	ctx context.Context,
	md metadata,
	mapping map[string]arcanav1.ValueSource,
) (*unstructured.Unstructured, error) {
	for _, source := range mapping {
		if source.Source == arcanav1.SourceClaimParam {
			return r.claimObject(ctx, md)
		}
	}

	return nil, nil
}

// ensureTargetFinalizer adds the finalizer if it is not already there.
//
// A merge patch rather than an apply, because an apply configuration cannot
// say "add one entry to this list" without claiming the whole list, and the
// list belongs to whoever else has put a finalizer on this object.
func (r *ArcanumManager) ensureTargetFinalizer(ctx context.Context, arcanum *arcanav1.Arcanum) error {
	base := arcanum.DeepCopy()

	if !controllerutil.AddFinalizer(arcanum, targetFinalizer) {
		return nil
	}

	return r.Patch(ctx, arcanum, client.MergeFrom(base))
}

// failedPhase reports err on the status rather than to the queue, keeping
// whatever the pass had already recorded.
func failedPhase(want phase, err error) phase {
	want.Phase = arcanav1.ArcanumPhaseFailed
	want.Message = err.Error()

	return want
}

// targetSecretMapFunc maps a Secret back to the Arcanum that wrote it.
//
// A target in the claim namespace carries no owner reference, so Owns has
// nothing to follow and only this makes an edited or deleted Secret heal
// itself.
func targetSecretMapFunc(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()

	name, namespace := labels[arcanumNameLabel], labels[arcanumNamespaceLabel]
	if name == "" || namespace == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	}}
}

// SetupWithManager wires the controller. The Owns lines cover the gather Job,
// its ConfigMaps, the gathered Secret and a target in the Arcanum's own
// namespace. The plain Secret watch is there for the one case an owner
// reference cannot express, a target in the claim namespace.
func (r *ArcanumManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&arcanav1.Arcanum{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(targetSecretMapFunc)).
		Complete(r)
}
