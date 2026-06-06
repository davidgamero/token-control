package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterTokenPolicySpec defines cluster-wide default LLM governance that applies to
// namespaces selected by NamespaceSelector. It is the broadest tier of the policy
// hierarchy: namespace TokenPolicies may only narrow (never widen) what is permitted here.
type ClusterTokenPolicySpec struct {
	// NamespaceSelector limits which namespaces inherit these defaults. An empty selector
	// selects all namespaces. A namespace is governed by every ClusterTokenPolicy whose
	// selector it matches.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// Models is the default model allowlist applied to selected namespaces. Workloads in
	// those namespaces may use only the union of Allowed models here (minus any Deny),
	// further narrowed by namespace/workload TokenPolicies.
	// +optional
	Models []ModelPermission `json:"models,omitempty"`

	// Quota is the default per-namespace token budget. Namespace policies may set a
	// smaller, but not larger, quota.
	// +optional
	Quota *TokenQuota `json:"quota,omitempty"`

	// Enforcement is the default enforcement mode for selected namespaces.
	// +kubebuilder:default=Enforce
	// +optional
	Enforcement EnforcementMode `json:"enforcement,omitempty"`

	// DefaultCredentialRef is the fallback ModelCredential used when a model permission
	// does not name its own credential.
	// +optional
	DefaultCredentialRef *CredentialReference `json:"defaultCredentialRef,omitempty"`

	// Gateway configures default downstream gateway artifact generation.
	// +optional
	Gateway *GatewayIntegration `json:"gateway,omitempty"`
}

// ClusterTokenPolicyStatus reports the observed state of a ClusterTokenPolicy.
type ClusterTokenPolicyStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ModelCount is the number of model permission rules in the spec.
	// +optional
	ModelCount int `json:"modelCount,omitempty"`

	// AppliedNamespaces lists namespaces currently matched by NamespaceSelector.
	// +optional
	AppliedNamespaces []string `json:"appliedNamespaces,omitempty"`

	// Conditions represent the latest available observations of the policy's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterTokenPolicy is the Schema for the cluster-scoped, default LLM governance policy.
// It is the cluster analogue of LimitRange/ResourceQuota for LLM access.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ctp,categories=governance;llm
// +kubebuilder:printcolumn:name="Enforcement",type=string,JSONPath=`.spec.enforcement`
// +kubebuilder:printcolumn:name="Models",type=integer,JSONPath=`.status.modelCount`
// +kubebuilder:printcolumn:name="Namespaces",type=integer,JSONPath=`.status.appliedNamespaces`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterTokenPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterTokenPolicySpec   `json:"spec,omitempty"`
	Status ClusterTokenPolicyStatus `json:"status,omitempty"`
}

// ClusterTokenPolicyList contains a list of ClusterTokenPolicy.
//
// +kubebuilder:object:root=true
type ClusterTokenPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterTokenPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterTokenPolicy{}, &ClusterTokenPolicyList{})
}
