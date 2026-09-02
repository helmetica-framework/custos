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

// ArcanumSpec describes one Secret custos assembles: where every value comes
// from, and what the Secret is called.
type ArcanumSpec struct {
	// Suspend pauses reconciliation. A suspended arcanum holds at Pending and
	// custos leaves everything it owns untouched, which is the escape hatch
	// when the controller and the cluster disagree.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// Target names the Secret. It is required because a Secret custos cannot
	// name is a Secret nobody can consume.
	// +required
	Target TargetSpec `json:"target"`

	// Credentials holds the value mapping. An arcanum with none is legal and
	// produces an empty Secret, which is how a chart can declare the Secret
	// before it knows what goes in it.
	// +optional
	Credentials CredentialsSpec `json:"credentials,omitempty"`
}

// TargetSpec names the Secret. The namespace is not settable: it follows from
// whether the Arcanum's own namespace carries chryso's claim annotation, so a
// reagent chart works both under chrysopoeia and under a plain helm install.
type TargetSpec struct {
	// Name is the Secret's name in whichever namespace the placement rule
	// picks.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// CredentialsSpec holds the mapping. Every entry becomes a key in the target
// Secret.
type CredentialsSpec struct {
	// ValueMapping is keyed by the Secret key to produce. Consumers read these
	// names, so they are the chart author's to choose, not derived from
	// wherever the value happened to come from.
	// +optional
	ValueMapping map[string]ValueSource `json:"valueMapping,omitempty"`
}

// SourceType selects which of ValueSource's fields apply.
// +kubebuilder:validation:Enum=const;objectRef;claimParam;exec;template
type SourceType string

const (
	// SourceConst is a literal custos copies through untouched.
	SourceConst SourceType = "const"
	// SourceObjectRef reads another object in the instance namespace. It needs
	// the gather Job, because custos itself has no read access to arbitrary
	// kinds.
	SourceObjectRef SourceType = "objectRef"
	// SourceClaimParam reads the claim CR that caused this instance to exist.
	SourceClaimParam SourceType = "claimParam"
	// SourceExec runs a command in a pod. It needs the gather Job, because
	// custos itself has no pods/exec.
	SourceExec SourceType = "exec"
	// SourceTemplate builds a value out of the other values, so a connection
	// string does not have to be assembled by every consumer.
	SourceTemplate SourceType = "template"
)

// ValueSource says where one key's value comes from. It is flat rather than a
// union of sub-structs because a chart author has to write this by hand, and
// the CEL rules below keep the wrong combinations out.
// +kubebuilder:validation:XValidation:rule="self.source != 'const' || has(self.value)",message="a const source needs a value"
// +kubebuilder:validation:XValidation:rule="self.source != 'template' || has(self.value)",message="a template source needs a value"
// +kubebuilder:validation:XValidation:rule="self.source != 'claimParam' || has(self.path)",message="a claimParam source needs a path"
// +kubebuilder:validation:XValidation:rule="self.source != 'objectRef' || (has(self.kind) && has(self.name))",message="an objectRef source needs a kind and a name"
// +kubebuilder:validation:XValidation:rule="self.source != 'objectRef' || (has(self.key) != has(self.path))",message="an objectRef source needs exactly one of key or path"
// +kubebuilder:validation:XValidation:rule="self.source != 'exec' || (has(self.podSelector) && has(self.command))",message="an exec source needs a podSelector and a command"
// +kubebuilder:validation:XValidation:rule="!has(self.podWait) || self.source == 'exec'",message="only an exec source has a podWait"
type ValueSource struct {
	// Source decides which of the fields below custos reads. There is no
	// default: guessing from which fields are set would turn a typo into a
	// different lookup.
	// +required
	Source SourceType `json:"source"`

	// Value is the literal for a const source, or the template body for a
	// template source.
	// +optional
	Value string `json:"value,omitempty"`

	// APIVersion is the group and version of an objectRef's target. It
	// defaults to the core group, since most references are to a Secret or a
	// ConfigMap.
	// +kubebuilder:default="v1"
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// Kind is the kind of an objectRef's target.
	// +optional
	Kind string `json:"kind,omitempty"`
	// Name is the object's name. It is rendered against the metadata context
	// first, so "{{.ClaimName}}-app" works.
	// +optional
	Name string `json:"name,omitempty"`
	// Key reads one entry out of a Secret's or ConfigMap's data. A Secret's
	// value is decoded.
	// +optional
	Key string `json:"key,omitempty"`
	// Path is a JSONPath, used by objectRef for anything that is not a Secret
	// or ConfigMap data key, and by claimParam against the claim.
	// +optional
	Path string `json:"path,omitempty"`

	// PodSelector picks the pod an exec source runs in. It is a selector
	// rather than a name because pod names are generated and change on every
	// restart.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
	// Container is which one to run in. Left empty, the pod's
	// kubectl.kubernetes.io/default-container annotation decides, and failing
	// that its first container.
	//
	// The API server only defaults this itself when the pod has exactly one
	// container, so for anything with a sidecar the choice has to be made
	// before the request goes out.
	// +optional
	Container string `json:"container,omitempty"`
	// PodWait is how long to wait for a pod matching PodSelector to become
	// ready before failing the gather. It exists because a service that is
	// slow to start is the ordinary case, not a fault, and waiting inside one
	// attempt is cheaper than failing and being retried.
	//
	// Unset falls back to the gather binary's own default. Changing it moves
	// the plan hash, so it re-runs a gather that has already settled.
	// +optional
	PodWait *metav1.Duration `json:"podWait,omitempty"`

	// Command's arguments are rendered against the metadata context.
	//
	// It must be idempotent: safe to run any number of times, returning the
	// same value each time. A gather is retried until it succeeds, so a command
	// that mints a fresh password per run would rewrite the target Secret under
	// consumers that have already read it. Provisioning is fine as long as it
	// is written to survive repetition.
	// +optional
	Command []string `json:"command,omitempty"`
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
	// GatheredHash is the hash of the plan the gathered values in hand came
	// from. A later reconcile computing the same hash knows it can skip the
	// Job, which is what keeps an exec that provisions from running twice.
	// +optional
	GatheredHash string `json:"gatheredHash,omitempty"`
	// GatherJobName is the Job custos is currently waiting on. It is recorded
	// so an operator chasing a stuck arcanum has somewhere to read logs.
	// +optional
	GatherJobName string `json:"gatherJobName,omitempty"`
	// SecretName is the Secret custos wrote.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// SecretNamespace is where that Secret landed. It is reported because the
	// placement rule is not something a user should have to re-derive.
	// +optional
	SecretNamespace string `json:"secretNamespace,omitempty"`
}

// Arcanum is the thing custos keeps.
// +kubebuilder:object:root=true
// +kubebuilder:ac:generate=true
// +kubebuilder:subresource:status
// The pluralizer would make this "arcanums", so the Latin plural is spelled out.
// +kubebuilder:resource:path=arcana,singular=arcanum
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.status.secretName`
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
