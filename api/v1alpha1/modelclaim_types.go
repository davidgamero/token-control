package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelClaimPhase is the high-level binding state of a ModelClaim.
// +kubebuilder:validation:Enum=Pending;Bound;Denied
type ModelClaimPhase string

const (
	// ClaimPending means the claim has not yet been resolved against the policy hierarchy.
	ClaimPending ModelClaimPhase = "Pending"
	// ClaimBound means every requested model is permitted by the effective policy.
	ClaimBound ModelClaimPhase = "Bound"
	// ClaimDenied means at least one requested model is not permitted by the effective policy.
	ClaimDenied ModelClaimPhase = "Denied"
)

// ModelRequest is a single provider/model a workload declares it intends to call.
type ModelRequest struct {
	// Provider is the LLM provider identifier, e.g. "openai", "anthropic", "bedrock".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Provider string `json:"provider"`

	// Model is the concrete model name the workload will call, e.g. "gpt-4o-mini".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Model string `json:"model"`

	// CredentialRef optionally requests a specific ModelCredential for this model. It is a
	// preference only: the policy hierarchy still authorizes the model and a policy that binds
	// its own credential takes precedence. Used as the fallback credential when the resolved
	// policy does not name one.
	// +optional
	CredentialRef *CredentialReference `json:"credentialRef,omitempty"`
}

// ModelClaimSpec is a workload team's declaration of intent: "these workloads will call these
// models". It is the bottom-up counterpart to the top-down grant expressed by
// ClusterTokenPolicy/TokenPolicy -- the LLM analogue of a PersistentVolumeClaim against the
// available supply. The claim is bound only insofar as the policy hierarchy permits the
// requested models; the controller records the per-model verdict in status.
type ModelClaimSpec struct {
	// Selector identifies the workloads in this namespace that the claim covers. An empty or
	// omitted selector covers every workload in the namespace. Binding the declaration to
	// workload identity (service accounts / pod labels) rather than a pod-authored annotation
	// is what makes it strongly typed and tamper-resistant: pods declare nothing.
	// +optional
	Selector *WorkloadSelector `json:"selector,omitempty"`

	// Models is the set of provider/model combinations the covered workloads intend to call.
	// +kubebuilder:validation:MinItems=1
	Models []ModelRequest `json:"models"`

	// Purpose is a free-form description of why these models are needed, recorded for auditing.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Purpose string `json:"purpose,omitempty"`
}

// ModelClaimStatus reports the resolved binding state of a ModelClaim.
type ModelClaimStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase summarizes binding: Bound when every requested model is permitted, Denied when any
	// requested model is refused by the effective policy, Pending before first resolution.
	// +optional
	Phase ModelClaimPhase `json:"phase,omitempty"`

	// ResolvedModels is the per-request verdict (Allow/Deny, bound credential, denying source)
	// computed from the policy hierarchy for the claim's scope.
	// +optional
	ResolvedModels []EffectiveModel `json:"resolvedModels,omitempty"`

	// BoundCredentials lists the distinct ModelCredentials resolved for the permitted models.
	// +optional
	BoundCredentials []string `json:"boundCredentials,omitempty"`

	// Conditions represent the latest available observations of the claim's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelClaim is the Schema for a workload's strongly-typed, validated declaration of the
// models it intends to call. It replaces the self-asserted governance.tokencontrol.io/models
// pod annotation: a claim is a first-class, RBAC-controlled, schema-validated object that the
// webhook resolves at create time and the controller binds against the policy hierarchy.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mcl,categories=governance;llm
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.spec.purpose`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelClaimSpec   `json:"spec,omitempty"`
	Status ModelClaimStatus `json:"status,omitempty"`
}

// ModelClaimList contains a list of ModelClaim.
//
// +kubebuilder:object:root=true
type ModelClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelClaim{}, &ModelClaimList{})
}
