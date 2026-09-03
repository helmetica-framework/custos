// Package v1 contains API Schema definitions for the arcana.helmetica.io v1 API group
// +kubebuilder:object:generate=true
// +kubebuilder:ac:generate=true
// +kubebuilder:ac:output:package="../../applyconfiguration"
// +groupName=arcana.helmetica.io
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "arcana.helmetica.io", Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	// SchemeGroupVersion is an alias for GroupVersion. The generated apply
	// configurations refer to it by this name, which is what client-gen
	// scaffolding calls it.
	SchemeGroupVersion = GroupVersion
)
