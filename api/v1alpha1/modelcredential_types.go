package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InjectionMode controls how a resolved credential is delivered to a workload.
// +kubebuilder:validation:Enum=None;Env;ProjectedVolume
type InjectionMode string

const (
	// InjectNone performs validation/binding bookkeeping but does not mutate pods.
	InjectNone InjectionMode = "None"
	// InjectEnv injects an environment variable sourced from the synced Secret.
	InjectEnv InjectionMode = "Env"
	// InjectProjectedVolume mounts the synced Secret as a projected volume.
	InjectProjectedVolume InjectionMode = "ProjectedVolume"
)

// SecretKeySelector references a key within a Secret. If Namespace is empty the operator
// namespace is assumed. The controller treats this Secret as the single source of truth and
// replicates it (never the raw value) into authorized workload namespaces.
type SecretKeySelector struct {
	// Name of the source Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace of the source Secret. Defaults to the operator namespace when empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Key within the Secret holding the API key/token.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// CredentialInjection describes how the mutating webhook injects this credential.
type CredentialInjection struct {
	// Mode selects the injection mechanism. Defaults to Env.
	// +kubebuilder:default=Env
	// +optional
	Mode InjectionMode `json:"mode,omitempty"`
	// EnvName overrides the environment variable name used when Mode=Env. When empty a
	// provider-appropriate default is used (e.g. OPENAI_API_KEY, ANTHROPIC_API_KEY).
	// +optional
	EnvName string `json:"envName,omitempty"`
	// MountPath overrides the mount path used when Mode=ProjectedVolume.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// ModelCredentialSpec binds an upstream provider credential to the set of namespaces and
// models permitted to use it. It exists so that workload teams declare intent ("use the
// openai credential") without each team managing and rotating their own Secret.
type ModelCredentialSpec struct {
	// Provider this credential authenticates against, e.g. "openai", "anthropic".
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// Models lists glob patterns this credential is valid for. Empty means all models for
	// the provider.
	// +optional
	Models []string `json:"models,omitempty"`

	// SecretRef points to the single source-of-truth Secret holding the API key.
	SecretRef SecretKeySelector `json:"secretRef"`

	// AllowedNamespaces lists namespaces explicitly permitted to bind this credential.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`

	// NamespaceSelector selects additional namespaces permitted to bind this credential.
	// A namespace is authorized if it is in AllowedNamespaces OR matches NamespaceSelector.
	// If both are empty, no namespace is authorized (deny by default).
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// Injection controls how the credential is delivered to authorized workloads.
	// +optional
	Injection *CredentialInjection `json:"injection,omitempty"`
}

// ModelCredentialStatus reports the observed state of a ModelCredential.
type ModelCredentialStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SecretResolved is true when the source Secret and key were found.
	// +optional
	SecretResolved bool `json:"secretResolved,omitempty"`

	// SyncedNamespaces lists namespaces into which the credential Secret has been replicated.
	// +optional
	SyncedNamespaces []string `json:"syncedNamespaces,omitempty"`

	// ReferencingPolicies lists policies (namespace/name) that reference this credential.
	// +optional
	ReferencingPolicies []string `json:"referencingPolicies,omitempty"`

	// Conditions represent the latest available observations of the credential's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelCredential is the Schema for cluster-scoped, centrally-managed provider credentials.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mc,categories=governance;llm
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="SecretResolved",type=boolean,JSONPath=`.status.secretResolved`
// +kubebuilder:printcolumn:name="Synced",type=integer,JSONPath=`.status.syncedNamespaces`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelCredentialSpec   `json:"spec,omitempty"`
	Status ModelCredentialStatus `json:"status,omitempty"`
}

// ModelCredentialList contains a list of ModelCredential.
//
// +kubebuilder:object:root=true
type ModelCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelCredential `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelCredential{}, &ModelCredentialList{})
}
