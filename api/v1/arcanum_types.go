package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ArcanumPhase string

const (
	ArcanumPhasePending ArcanumPhase = "Pending"
	ArcanumPhaseReady   ArcanumPhase = "Ready"
	ArcanumPhaseFailed  ArcanumPhase = "Failed"
)

// ArcanumSpec describes what custos guards. It is deliberately near-empty: the
// real fields land here once the resource custos watches over is settled.
type ArcanumSpec struct {
	// Suspend pauses reconciliation. A suspended arcanum holds at Pending and
	// custos leaves everything it owns untouched, which is the escape hatch
	// when the controller and the cluster disagree.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`
}

type ArcanumStatus struct {
	// +optional
	Phase ArcanumPhase `json:"phase,omitempty"`
	// ObservedGeneration is the spec generation the phase was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Message explains a Pending or Failed phase.
	// +optional
	Message string `json:"message,omitempty"`
}

// Arcanum is the thing custos keeps.
// +kubebuilder:object:root=true
// +kubebuilder:ac:generate=true
// +kubebuilder:subresource:status
// The pluralizer would make this "arcanums", so the Latin plural is spelled out.
// +kubebuilder:resource:path=arcana,singular=arcanum
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
type Arcanum struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ArcanumSpec   `json:"spec,omitempty"`
	Status ArcanumStatus `json:"status,omitempty"`
}

// ArcanumList contains a list of Arcanum.
// +kubebuilder:object:root=true
type ArcanumList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Arcanum `json:"items"`
}

func init() { SchemeBuilder.Register(&Arcanum{}, &ArcanumList{}) }
