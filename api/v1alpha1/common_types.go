package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Annotation and label keys consumed and produced by the controller and webhooks.
const (
	// AnnotationModels is set by workload authors to declare which provider/model
	// combinations the workload intends to call, as a comma-separated list of
	// "provider/model" tokens, e.g. "openai/gpt-4,anthropic/claude-3-5-sonnet".
	// The admission webhook validates these declarations against the effective policy.
	AnnotationModels = "governance.tokencontrol.io/models"

	// AnnotationEffectivePolicy is written by the mutating webhook onto admitted pods
	// to record the policies that produced the effective decision (for auditing).
	AnnotationEffectivePolicy = "governance.tokencontrol.io/effective-policy"

	// AnnotationCredentialsBound is written by the mutating webhook to record which
	// ModelCredentials were injected into the pod.
	AnnotationCredentialsBound = "governance.tokencontrol.io/credentials-bound"

	// AnnotationInjectionDisabled, when set to "true" on a pod, opts the pod out of
	// credential injection (validation still applies unless the namespace is exempt).
	AnnotationInjectionDisabled = "governance.tokencontrol.io/inject-credentials-disabled"

	// LabelManagedBy marks objects created/owned by the controller (e.g. synced secrets,
	// generated gateway policies).
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// LabelCredential marks a synced credential Secret with the source ModelCredential name.
	LabelCredential = "governance.tokencontrol.io/credential"

	// ManagedByValue is the value used for LabelManagedBy on owned objects.
	ManagedByValue = "token-control"

	// ManagedSecretPrefix is prepended to synced credential Secret names in workload namespaces.
	ManagedSecretPrefix = "tc-cred-"
)

// EnforcementMode determines how the controller and webhooks react to a policy violation.
// +kubebuilder:validation:Enum=Enforce;Audit;Disabled
type EnforcementMode string

const (
	// EnforcementEnforce rejects non-conforming workloads at admission and emits metrics/events.
	EnforcementEnforce EnforcementMode = "Enforce"
	// EnforcementAudit admits non-conforming workloads but emits warnings, events and metrics.
	EnforcementAudit EnforcementMode = "Audit"
	// EnforcementDisabled performs no admission decisions for the affected scope.
	EnforcementDisabled EnforcementMode = "Disabled"
)

// PermissionAction declares whether a ModelPermission grants or revokes access.
// +kubebuilder:validation:Enum=Allow;Deny
type PermissionAction string

const (
	// ActionAllow permits the matching provider/model.
	ActionAllow PermissionAction = "Allow"
	// ActionDeny forbids the matching provider/model. Deny always overrides Allow.
	ActionDeny PermissionAction = "Deny"
)

// ModelPermission declares a provider/model combination and whether it is permitted.
// Model supports glob patterns ("gpt-4o-*", "*"). Deny rules always win over Allow
// rules at the same or a broader scope.
type ModelPermission struct {
	// Provider is the LLM provider identifier, e.g. "openai", "anthropic", "bedrock", "vertex".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Provider string `json:"provider"`

	// Model is a model name or glob pattern, e.g. "gpt-4", "gpt-4o-*", "*".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Model string `json:"model"`

	// Action is whether to Allow or Deny this provider/model. Defaults to Allow.
	// +kubebuilder:default=Allow
	// +optional
	Action PermissionAction `json:"action,omitempty"`

	// CredentialRef binds a specific ModelCredential to this permission. When set, the
	// mutating webhook injects that credential for workloads using this model.
	// +optional
	CredentialRef *CredentialReference `json:"credentialRef,omitempty"`

	// Quota optionally narrows the token budget for this specific provider/model.
	// It may only be more restrictive than the enclosing scope's quota.
	// +optional
	Quota *TokenQuota `json:"quota,omitempty"`
}

// TokenQuota expresses token and request budgets across rolling/calendar windows.
// All fields are optional; an unset field means "no additional limit at this scope".
type TokenQuota struct {
	// TokensPerMinute caps tokens consumed in a rolling 60s window (TPM).
	// +optional
	// +kubebuilder:validation:Minimum=0
	TokensPerMinute *int64 `json:"tokensPerMinute,omitempty"`

	// RequestsPerMinute caps the number of requests in a rolling 60s window (RPM).
	// +optional
	// +kubebuilder:validation:Minimum=0
	RequestsPerMinute *int64 `json:"requestsPerMinute,omitempty"`

	// TokensPerDay caps tokens consumed per UTC calendar day.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TokensPerDay *int64 `json:"tokensPerDay,omitempty"`

	// TokensPerMonth caps tokens consumed per UTC calendar month.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TokensPerMonth *int64 `json:"tokensPerMonth,omitempty"`
}

// CredentialReference references a cluster-scoped ModelCredential by name.
type CredentialReference struct {
	// Name of the ModelCredential.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// WorkloadSelector targets a subset of workloads within a namespace. An empty
// selector (or an unset Selector on a TokenPolicy) matches every workload in the
// namespace and therefore acts as the namespace default.
type WorkloadSelector struct {
	// PodSelector matches pods by labels. Empty matches all pods in the namespace.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// ServiceAccounts restricts the policy to pods running as any of these service accounts.
	// +optional
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`
}

// IsEmpty reports whether the selector targets all workloads (namespace default).
func (w *WorkloadSelector) IsEmpty() bool {
	if w == nil {
		return true
	}
	if len(w.ServiceAccounts) > 0 {
		return false
	}
	if w.PodSelector == nil {
		return true
	}
	return len(w.PodSelector.MatchLabels) == 0 && len(w.PodSelector.MatchExpressions) == 0
}

// GatewayType enumerates the supported downstream gateway integrations for which the
// controller can generate rate-limit artifacts.
// +kubebuilder:validation:Enum=None;EnvoyAIGateway;Kuadrant
type GatewayType string

const (
	// GatewayNone disables gateway artifact generation (admission-only governance).
	GatewayNone GatewayType = "None"
	// GatewayEnvoyAIGateway generates Envoy Gateway BackendTrafficPolicy objects.
	GatewayEnvoyAIGateway GatewayType = "EnvoyAIGateway"
	// GatewayKuadrant annotates/targets Kuadrant TokenRateLimitPolicy objects.
	GatewayKuadrant GatewayType = "Kuadrant"
)

// GatewayIntegration configures generation of downstream gateway rate-limit artifacts
// from a policy's quota. This keeps the controller out of the request hot path while
// still allowing live token enforcement at an existing gateway.
type GatewayIntegration struct {
	// Type selects the gateway flavor. Defaults to None.
	// +kubebuilder:default=None
	// +optional
	Type GatewayType `json:"type,omitempty"`

	// TargetRef identifies the gateway route/backend to which generated policy attaches.
	// +optional
	TargetRef *GatewayTargetRef `json:"targetRef,omitempty"`
}

// GatewayTargetRef is a typed reference to a gateway object that generated policy targets.
type GatewayTargetRef struct {
	// Group of the target, e.g. "gateway.networking.k8s.io" or "aigateway.envoyproxy.io".
	// +optional
	Group string `json:"group,omitempty"`
	// Kind of the target, e.g. "HTTPRoute", "AIGatewayRoute".
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// Name of the target object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace of the target. Defaults to the policy namespace for namespaced policies.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// EffectiveModel is a fully-resolved model permission recorded in policy status.
type EffectiveModel struct {
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Action   PermissionAction `json:"action"`
	// Credential is the bound ModelCredential name, if any.
	// +optional
	Credential string `json:"credential,omitempty"`
	// Source identifies the policy (kind/namespace/name) that contributed this entry.
	// +optional
	Source string `json:"source,omitempty"`
}

// UsageStatus records observed token/request consumption for a scope. These counters are
// advisory and observability-oriented: they are fed by an external usage reporter (e.g. a
// gateway) and exposed via metrics. They are NOT the request-time enforcement mechanism.
type UsageStatus struct {
	// WindowStart is the start of the current accounting window.
	// +optional
	WindowStart *metav1.Time `json:"windowStart,omitempty"`
	// TokensMinute is tokens consumed in the current rolling minute.
	// +optional
	TokensMinute int64 `json:"tokensMinute,omitempty"`
	// TokensDay is tokens consumed in the current UTC day.
	// +optional
	TokensDay int64 `json:"tokensDay,omitempty"`
	// TokensMonth is tokens consumed in the current UTC month.
	// +optional
	TokensMonth int64 `json:"tokensMonth,omitempty"`
	// RequestsMinute is requests counted in the current rolling minute.
	// +optional
	RequestsMinute int64 `json:"requestsMinute,omitempty"`
	// LastUpdated is when these counters were last refreshed.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

// Common condition types used across the API.
const (
	// ConditionReady indicates the policy/credential has been reconciled successfully.
	ConditionReady = "Ready"
	// ConditionValid indicates the spec passed semantic validation against cluster state.
	ConditionValid = "Valid"
	// ConditionGatewaySynced indicates generated gateway artifacts are up to date.
	ConditionGatewaySynced = "GatewaySynced"
	// ConditionSecretResolved indicates a ModelCredential's source Secret was found.
	ConditionSecretResolved = "SecretResolved"
	// ConditionOversubscribed indicates a ModelCredential's allocated token budgets exceed
	// its declared capacity (a planning signal; it does not block reconciliation).
	ConditionOversubscribed = "Oversubscribed"
)
