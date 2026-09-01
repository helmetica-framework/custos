package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
)

// targetFinalizer holds the Arcanum open until its Secret is dealt with. A
// target in the claim namespace carries no owner reference, so without this
// nothing would ever remove it.
const targetFinalizer = "custos.helmetica.io/target-secret"

// controllerRef is the owner reference custos puts on what it creates.
// controllerutil.SetControllerReference cannot be used, because it works on
// objects and everything here is an apply configuration.
//
// Controller is set because Owns only enqueues for the controller owner, so
// without it garbage collection still works while the watch that heals an
// edited object never fires.
func controllerRef(arcanum *arcanav1.Arcanum) *metav1ac.OwnerReferenceApplyConfiguration {
	return &metav1ac.OwnerReferenceApplyConfiguration{
		APIVersion: ptr.To(arcanav1.GroupVersion.String()),
		Kind:       ptr.To("Arcanum"),
		Name:       ptr.To(arcanum.GetName()),
		UID:        ptr.To(arcanum.GetUID()),
		Controller: ptr.To(true),
	}
}

// applyTargetSecret writes the credentials. local says the target is the
// Arcanum's own namespace, which is the only case where an owner reference is
// possible: a reference across namespaces is not resolvable, and the garbage
// collector deletes the object that carries one.
//
// A Secret that already exists under someone else's ownership is refused
// rather than overwritten, since the target name is a chart author's free
// choice and can collide with something a tenant put there first.
func (r *ArcanumManager) applyTargetSecret(
	ctx context.Context,
	arcanum *arcanav1.Arcanum,
	ns string,
	local bool,
	data map[string]string,
) error {
	name := arcanum.Spec.Target.Name

	existing := &corev1.Secret{}

	// Uncached on purpose. The manager cache holds only Secrets carrying
	// arcanumNameLabel, so a foreign one reads as NotFound and this check
	// would pass exactly when it has to fail.
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting secret %s/%s: %w", ns, name, err)
	}

	// Only a Secret that is already there can belong to someone else. A
	// NotFound is the ordinary first write.
	if err == nil && existing.GetLabels()[arcanumUIDLabel] != string(arcanum.GetUID()) {
		return fmt.Errorf("secret %s/%s exists and is not managed by this arcanum", ns, name)
	}

	// StringData rather than Data, so nothing here hand encodes base64. The
	// API server moves it into data on write.
	secret := corev1ac.Secret(name, ns).
		WithType(corev1.SecretTypeOpaque).
		WithLabels(ownershipLabels(arcanum)).
		WithStringData(data)

	if local {
		secret.WithOwnerReferences(controllerRef(arcanum))
	}

	if err := r.Apply(ctx, secret, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying secret %s/%s: %w", ns, name, err)
	}

	return nil
}

// cleanupTargetSecret removes the Secret recorded in the status, if it is
// still ours.
//
// Every reason this might not finish is a success. It backs a finalizer, and
// a finalizer that returns an error on something it can never fix leaves the
// Arcanum undeletable and its namespace stuck in Terminating.
func (r *ArcanumManager) cleanupTargetSecret(ctx context.Context, arcanum *arcanav1.Arcanum) error {
	// Nothing was ever written, so there is nothing to take back.
	if arcanum.Status.SecretName == "" {
		return nil
	}

	ns, name := arcanum.Status.SecretNamespace, arcanum.Status.SecretName

	secret := &corev1.Secret{}

	// Uncached for the same reason as the apply path.
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			// Gone, or in a namespace already Terminating. Either way
			// there is nothing left to delete.
			r.Log.V(1).Info("target secret is already gone", "namespace", ns, "name", name)

			return nil
		}

		return fmt.Errorf("getting secret %s/%s: %w", ns, name, err)
	}

	// A name and namespace can be reused by a later Arcanum, so the UID is
	// the only thing that says this is the Secret custos wrote. Deleting
	// someone else's would turn a name collision into data loss.
	if secret.GetLabels()[arcanumUIDLabel] != string(arcanum.GetUID()) {
		return nil
	}

	err = r.Delete(ctx, secret)
	if err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
			return fmt.Errorf("deleting secret %s/%s: %w", ns, name, err)
		}

		// Someone else got there between the read and the delete.
		r.Log.V(1).Info("target secret went away before it could be deleted", "namespace", ns, "name", name)
	}

	return nil
}
