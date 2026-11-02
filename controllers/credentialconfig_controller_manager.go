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

	custosv1 "github.com/helmetica-framework/custos/api/v1"
	custosacv1 "github.com/helmetica-framework/custos/applyconfiguration/api/v1"
)

const (
	credentialsAppliedMessage = "credentials applied"
	gatheringMessage          = "gathering values"
	suspendedMessage          = "reconciliation suspended"
)

// CredentialConfigManager reconciles CredentialConfig objects.
type CredentialConfigManager struct {
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
	Phase           custosv1.CredentialConfigPhase
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

// +kubebuilder:rbac:groups=custos.helmetica.io,resources=credentialconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=custos.helmetica.io,resources=credentialconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=custos.helmetica.io,resources=credentialconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete

// Reconcile drives an CredentialConfig's status to reflect desiredPhase, and takes the
// target Secret back when the CredentialConfig goes away.
//
// The deletion branch runs before anything else. A Secret in the claim
// namespace cannot carry an owner reference, so the finalizer is the only
// thing that removes it.
func (r *CredentialConfigManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("credentialconfig", req.NamespacedName)

	credentialConfig := &custosv1.CredentialConfig{}
	err := r.Get(ctx, req.NamespacedName, credentialConfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("credential config is gone, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.V(1).Info("reconciling",
		"generation", credentialConfig.Generation,
		"phase", credentialConfig.Status.Phase)

	if !credentialConfig.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("credential config is being deleted, cleaning finalizer")

		if controllerutil.ContainsFinalizer(credentialConfig, targetFinalizer) {
			err := r.cleanupTargetSecret(ctx, credentialConfig)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("removing target secret: %w", err)
			}
		}

		patch := client.MergeFromWithOptions(credentialConfig.DeepCopy(), client.MergeFromWithOptimisticLock{})

		// With the lock there's a chance for conflicts.
		if !controllerutil.RemoveFinalizer(credentialConfig, targetFinalizer) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, r.Patch(ctx, credentialConfig, patch)
	}

	want, err := r.desiredPhase(ctx, credentialConfig)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A retry lives here rather than in the status, so both exits below have to
	// carry it. Dropping it on the settled path leaves the CredentialConfig at Failed
	// with nothing left to wake it.
	result := ctrl.Result{RequeueAfter: want.RequeueAfter}

	if want.RequeueAfter > 0 {
		log.V(1).Info("scheduling another look", "after", want.RequeueAfter)
	}

	// Every recorded field is compared, or a decision that only moved the Job
	// name would be made every pass and written none of them.
	if credentialConfig.Status.Phase == want.Phase &&
		credentialConfig.Status.ObservedGeneration == credentialConfig.Generation &&
		credentialConfig.Status.Message == want.Message &&
		credentialConfig.Status.GatheredHash == want.GatheredHash &&
		credentialConfig.Status.GatherJobName == want.GatherJobName &&
		credentialConfig.Status.SecretName == want.SecretName &&
		credentialConfig.Status.SecretNamespace == want.SecretNamespace {
		return result, nil
	}

	if credentialConfig.Status.Phase != want.Phase {
		log.Info("credential config phase changed", "from", credentialConfig.Status.Phase, "to", want.Phase)
	}

	status := custosacv1.CredentialConfig(credentialConfig.GetName(), credentialConfig.GetNamespace()).
		WithStatus(custosacv1.CredentialConfigStatus().
			WithPhase(want.Phase).
			WithObservedGeneration(credentialConfig.Generation).
			WithMessage(want.Message).
			WithGatheredHash(want.GatheredHash).
			WithGatherJobName(want.GatherJobName).
			WithSecretName(want.SecretName).
			WithSecretNamespace(want.SecretNamespace))

	if err := r.Status().Apply(ctx, status, fieldOwner, client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying credential config status: %w", err)
	}

	return result, nil
}

// desiredPhase runs one pass of the whole flow and reports what the status
// should say. Everything it provisions sits behind the suspend check, so
// suspending an CredentialConfig leaves a Secret it already wrote in place.
//
// Most failures here are the phase rather than a returned error: a mapping
// that cannot render, a claim that is not there and a target name already
// taken are all things a retry cannot fix, and returning an error would have
// the queue back off over them forever.
func (r *CredentialConfigManager) desiredPhase(ctx context.Context, credentialConfig *custosv1.CredentialConfig) (phase, error) {
	// Carried forward, because a pass that holds or fails must not wipe what
	// an earlier one recorded. cleanupTargetSecret finds the Secret through
	// status.SecretName, so losing that would strand it.
	want := phase{
		GatheredHash:    credentialConfig.Status.GatheredHash,
		GatherJobName:   credentialConfig.Status.GatherJobName,
		SecretName:      credentialConfig.Status.SecretName,
		SecretNamespace: credentialConfig.Status.SecretNamespace,
	}

	log := r.credentialConfigLogger(credentialConfig)

	if ptr.Deref(credentialConfig.Spec.Suspend, false) {
		log.V(1).Info("suspended, leaving everything as it is")

		want.Phase = custosv1.CredentialConfigPhasePending
		want.Message = suspendedMessage

		return want, nil
	}

	md, hasClaim, err := r.instanceContext(ctx, credentialConfig)
	if err != nil {
		return failedPhase(want, err), nil
	}

	mapping := credentialConfig.Spec.Credentials.ValueMapping

	if err := validate(mapping, hasClaim); err != nil {
		log.Info("value mapping is invalid, nothing will be created", "reason", err.Error())

		return failedPhase(want, err), nil
	}

	// Before the write, not after. A delete landing in between would
	// otherwise leave the Secret behind in a namespace custos no longer has
	// any reason to visit.
	if hasClaim {
		if err := r.ensureTargetFinalizer(ctx, credentialConfig); err != nil {
			return want, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	var gathered map[string]string

	if needsGather(mapping) {
		values, hold, err := r.gatherValues(ctx, credentialConfig, md, mapping, &want)
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

	if err := r.applyTargetSecret(ctx, credentialConfig, ns, !hasClaim, rendered); err != nil {
		log.Info("target secret was not written", "namespace", ns, "reason", err.Error())

		return failedPhase(want, err), nil
	}

	log.V(1).Info("credentials applied",
		"namespace", ns, "name", credentialConfig.Spec.Target.Name, "keys", len(rendered))

	// Failing to clean up does not un-write the Secret, so it is reported and
	// tried again next pass rather than losing the Ready this pass earned.
	if err := r.deleteGatherJobs(ctx, credentialConfig); err != nil {
		log.Info("could not clean up the gather jobs", "reason", err.Error())
	} else {
		want.GatherJobName = ""
	}

	want.Phase = custosv1.CredentialConfigPhaseReady
	want.Message = credentialsAppliedMessage
	want.SecretName = credentialConfig.Spec.Target.Name
	want.SecretNamespace = ns

	return want, nil
}

// gatherValues runs the gather stage. It returns hold when the pass has to
// stop where it is, either waiting on a Job or reporting a failed one, having
// already written what to say into want.
func (r *CredentialConfigManager) gatherValues(
	ctx context.Context,
	credentialConfig *custosv1.CredentialConfig,
	md metadata,
	mapping map[string]custosv1.ValueSource,
	want *phase,
) (values map[string]string, hold bool, err error) {
	plan, err := buildPlan(mapping, md)
	if err != nil {
		*want = failedPhase(*want, err)

		return nil, true, nil
	}

	// The refresh annotation is hashed in, so changing it is what makes this
	// a different Job and re-runs a gather that has already settled.
	hash := gatherHash(plan, credentialConfig.GetAnnotations()[refreshAnnotation])
	jobName := gatherJobName(credentialConfig.GetName(), hash)

	log := r.credentialConfigLogger(credentialConfig).WithValues("job", jobName, "hash", hash)

	// Pruned before anything is decided, so a changed plan does not leave the
	// Job for the old one running alongside the new one.
	if err := r.deleteStaleGatherJobs(ctx, credentialConfig, jobName); err != nil {
		return nil, true, err
	}

	gathered, found, err := r.gatheredValues(ctx, credentialConfig)
	if err != nil {
		return nil, true, err
	}

	// The values in hand came from this very plan, so there is nothing to run.
	if found && credentialConfig.Status.GatheredHash == hash {
		log.V(1).Info("gathered values are current", "values", len(gathered))

		return gathered, false, nil
	}

	want.GatherJobName = jobName

	job := &batchv1.Job{}

	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: credentialConfig.GetNamespace()}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, true, fmt.Errorf("getting gather job: %w", err)
	}

	if apierrors.IsNotFound(err) {
		log.Info("starting gather job", "entries", len(plan.Entries))

		if err := r.startGather(ctx, credentialConfig, plan, hash); err != nil {
			return nil, true, err
		}

		want.Phase = custosv1.CredentialConfigPhasePending
		want.Message = gatheringMessage

		return nil, true, nil
	}

	// Above the switch, because a Job that gave up also has failed pods and
	// would otherwise be read as one that is still trying.
	if jobGaveUp(job) {
		want.Phase = custosv1.CredentialConfigPhaseFailed
		want.Message = r.gatherResultMessage(ctx, credentialConfig,
			fmt.Sprintf("gather job %s failed and left no result, see its logs", jobName))

		log.Info("gather job gave up", "attempts", job.Status.Failed, "reason", want.Message)

		return nil, true, r.retryGather(ctx, job, want)
	}

	switch {
	case job.Status.Succeeded > 0 && !found:
		// A Job that finished without leaving values either had its Secret
		// removed, or exited zero on a failure. It reports what went wrong
		// either way, and that beats rendering an empty credential.
		want.Phase = custosv1.CredentialConfigPhaseFailed
		want.Message = r.gatherResultMessage(ctx, credentialConfig,
			fmt.Sprintf("gather job %s succeeded but left no values", jobName))

		log.Info("gather job finished without values", "reason", want.Message)

		return nil, true, r.retryGather(ctx, job, want)

	case job.Status.Succeeded > 0:
		want.GatheredHash = hash

		log.Info("gather succeeded", "values", len(gathered))

		return gathered, false, nil

	case job.Status.Failed > 0:
		// An attempt failed and another is coming. The reason on a Pending is
		// more use than Failed at something that is about to fix itself.
		want.Phase = custosv1.CredentialConfigPhasePending
		want.Message = r.gatherResultMessage(ctx, credentialConfig, gatheringMessage)

		log.Info("gather attempt failed, another is coming",
			"attempts", job.Status.Failed, "reason", want.Message)

		return nil, true, nil

	default:
		log.V(1).Info("gather job is still running")

		want.Phase = custosv1.CredentialConfigPhasePending
		want.Message = gatheringMessage

		return nil, true, nil
	}
}

// credentialConfigLogger names the CredentialConfig every line below Reconcile is about. The
// controller-runtime request logger does not reach here, because desiredPhase
// takes the object rather than the request.
func (r *CredentialConfigManager) credentialConfigLogger(credentialConfig *custosv1.CredentialConfig) logr.Logger {
	return r.Log.WithValues("credentialconfig", client.ObjectKeyFromObject(credentialConfig))
}

// claimFor reads the claim only when the mapping actually needs it, so an
// CredentialConfig that never mentions the claim does not fail because that one object
// happens to be unreadable.
func (r *CredentialConfigManager) claimFor(
	ctx context.Context,
	md metadata,
	mapping map[string]custosv1.ValueSource,
) (*unstructured.Unstructured, error) {
	for _, source := range mapping {
		if source.Source == custosv1.SourceClaimParam {
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
func (r *CredentialConfigManager) ensureTargetFinalizer(ctx context.Context, credentialConfig *custosv1.CredentialConfig) error {
	base := credentialConfig.DeepCopy()

	if !controllerutil.AddFinalizer(credentialConfig, targetFinalizer) {
		return nil
	}

	return r.Patch(ctx, credentialConfig, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// failedPhase reports err on the status rather than to the queue, keeping
// whatever the pass had already recorded.
func failedPhase(want phase, err error) phase {
	want.Phase = custosv1.CredentialConfigPhaseFailed
	want.Message = err.Error()

	return want
}

// targetSecretMapFunc maps a Secret back to the CredentialConfig that wrote it.
//
// A target in the claim namespace carries no owner reference, so Owns has
// nothing to follow and only this makes an edited or deleted Secret heal
// itself.
func targetSecretMapFunc(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()

	name, namespace := labels[credentialConfigNameLabel], labels[credentialConfigNamespaceLabel]
	if name == "" || namespace == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	}}
}

// SetupWithManager wires the controller. The Owns lines cover the gather Job,
// its ConfigMaps, the gathered Secret and a target in the CredentialConfig's own
// namespace. The plain Secret watch is there for the one case an owner
// reference cannot express, a target in the claim namespace.
func (r *CredentialConfigManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&custosv1.CredentialConfig{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(targetSecretMapFunc)).
		Complete(r)
}
