package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
)

// The annotations chrysopoeia stamps on an instance namespace. They are the
// only thing that ties an Arcanum back to the claim it was provisioned for,
// so their spelling has to match chryso exactly.
const (
	claimNamespaceAnnotation  = "chrysopoeia.io/claim-namespace"
	claimNameAnnotation       = "chrysopoeia.io/claim-name"
	claimKindAnnotation       = "chrysopoeia.io/claim-kind"
	claimAPIVersionAnnotation = "chrysopoeia.io/claim-apiVersion"
)

// instanceContext reads the Arcanum's own namespace and works out whether it
// was provisioned by chrysopoeia. The second return is false for a plain helm
// install, which is a supported case and not an error: that Arcanum has no
// claim to point at and writes its Secret next to the service.
//
// InstanceNamespace and ArcanumName are filled in every case, claim or not.
// They are the only metadata a helm install has.
//
// A namespace carrying some of the four annotations but not all of them is an
// error naming the ones that are missing. Chryso writes the four together, so
// a partial set means they were edited by hand. Carrying on would look up a
// claim with an empty kind, and that failure reads as a broken lookup rather
// than as the half-stamped namespace it is.
func (r *ArcanumManager) instanceContext(ctx context.Context, arcanum *arcanav1.Arcanum) (metadata, bool, error) {
	md := metadata{
		InstanceNamespace: arcanum.GetNamespace(),
		ArcanumName:       arcanum.GetName(),
	}

	ns := &corev1.Namespace{}

	err := r.Get(ctx, client.ObjectKey{Name: arcanum.GetNamespace()}, ns)
	if err != nil {
		return md, false, fmt.Errorf("getting instance namespace: %w", err)
	}

	annotations := ns.GetAnnotations()

	names := []string{
		claimNamespaceAnnotation,
		claimNameAnnotation,
		claimKindAnnotation,
		claimAPIVersionAnnotation,
	}

	set := map[string]string{}
	missing := []string{}

	for _, name := range names {
		value := annotations[name]
		if value == "" {
			missing = append(missing, name)
			continue
		}
		set[name] = value
	}

	if len(set) == 0 {
		return md, false, nil
	}

	if len(missing) > 0 {
		return md, false, fmt.Errorf("instance namespace %s is missing annotations %s",
			ns.GetName(), strings.Join(missing, ", "))
	}

	md.ClaimNamespace = set[claimNamespaceAnnotation]
	md.ClaimName = set[claimNameAnnotation]
	md.ClaimKind = set[claimKindAnnotation]
	md.ClaimAPIVersion = set[claimAPIVersionAnnotation]

	return md, true, nil
}

// claimObject fetches the claim CR named by the metadata. Its GVK comes from
// md.ClaimAPIVersion and md.ClaimKind, which are only known at runtime.
//
// The get goes through APIReader, never Client. A cached read of a runtime
// kind would start an informer for every claim kind in the cluster.
func (r *ArcanumManager) claimObject(ctx context.Context, md metadata) (*unstructured.Unstructured, error) {
	claim := &unstructured.Unstructured{}

	gv, err := schema.ParseGroupVersion(md.ClaimAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing groupVersion: %w", err)
	}

	gvk := gv.WithKind(md.ClaimKind)

	claim.SetGroupVersionKind(gvk)

	err = r.APIReader.Get(ctx, client.ObjectKey{Name: md.ClaimName, Namespace: md.ClaimNamespace}, claim)
	if err != nil {
		return nil, fmt.Errorf("getting claim: %w", err)
	}

	return claim, nil
}

// targetNamespace is where the credentials Secret goes: beside the user's
// claim when there is one, and otherwise in the Arcanum's own namespace, next
// to the service.
func targetNamespace(md metadata, hasClaim bool) string {
	if hasClaim {
		return md.ClaimNamespace
	}
	return md.InstanceNamespace
}
