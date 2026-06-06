package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TokenPolicySpec defines namespace- or workload-scoped LLM governance. A TokenPolicy
// with an empty Selector is the namespace default; one with a Selector targets specific
// workloads and may only further constrain (never widen) the namespace default and the
// applicable ClusterTokenPolicies.
type TokenPolicySpec struct {
	// Selector targets workloads within this namespace. An empty/omitted selector makes
	// this the namespace-default policy. At most one namespace-default policy should exist
	// per namespace; if several do, the highest Priority wins.
	// +optional
	Selector *WorkloadSelector `json:"selector,omitempty"`

	// Models is the model allowlist for the targeted scope. Each Allow must be permitted by
	// the namespace default and by the applicable ClusterTokenPolicies; the validating
	// webhook rejects rules that attempt to widen the inherited permission set.
	// +optional
	Models []ModelPermission `json:"models,omitempty"`

	// Quota is the token budget for the targeted scope. It may only be more restrictive than
	// the inherited namespace/cluster quota.
	// +optional
	Quota *TokenQuota `json:"quota,omitempty"`

	// Enforcement overrides the inherited enforcement mode for the targeted scope. When empty,
	// the mode is inherited from the applicable ClusterTokenPolicy (default Enforce).
	// +optional
	Enforcement EnforcementMode `json:"enforcement,omitempty"`

	// Priority resolves overlapping policies that target the same workload; the highest value
	// wins. Defaults to 0.
	// +kubebuilder:default=0
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// Gateway overrides the inherited downstream gateway integration for this scope.
	// +optional
	Gateway *GatewayIntegration `json:"gateway,omitempty"`
}

// TokenPolicyStatus reports the observed and resolved state of a TokenPolicy.
type TokenPolicyStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// EffectiveModels is the fully-resolved model permission set after applying the policy
	// hierarchy (cluster -> namespace -> workload).
	// +optional
	EffectiveModels []EffectiveModel `json:"effectiveModels,omitempty"`

	// EffectiveQuota is the resolved (most restrictive) quota across the hierarchy.
	// +optional
	EffectiveQuota *TokenQuota `json:"effectiveQuota,omitempty"`

	// Usage holds advisory consumption counters for this scope.
	// +optional
	Usage *UsageStatus `json:"usage,omitempty"`

	// BoundCredentials lists the ModelCredentials resolved for this policy's models.
	// +optional
	BoundCredentials []string `json:"boundCredentials,omitempty"`

	// GatewayRef is the generated gateway artifact this policy currently manages, if any.
	// +optional
	GatewayRef string `json:"gatewayRef,omitempty"`

	// Conditions represent the latest available observations of the policy's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TokenPolicy is the Schema for namespace- and workload-scoped LLM governance.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tp,categories=governance;llm
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.selector`,priority=1
// +kubebuilder:printcolumn:name="Enforcement",type=string,JSONPath=`.spec.enforcement`
// +kubebuilder:printcolumn:name="TPM",type=integer,JSONPath=`.status.effectiveQuota.tokensPerMinute`
// +kubebuilder:printcolumn:name="Used(min)",type=integer,JSONPath=`.status.usage.tokensMinute`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TokenPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TokenPolicySpec   `json:"spec,omitempty"`
	Status TokenPolicyStatus `json:"status,omitempty"`
}

// TokenPolicyList contains a list of TokenPolicy.
//
// +kubebuilder:object:root=true
type TokenPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TokenPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TokenPolicy{}, &TokenPolicyList{})
}
