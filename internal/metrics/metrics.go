// Package metrics defines the Prometheus collectors exported by token-control. All
// collectors are registered against the controller-runtime metrics registry so they are
// served on the manager's metrics endpoint.
//
// Note: the token-consumption counters are advisory. token-control is not in the request
// hot path; an external usage reporter (typically the AI gateway) is expected to push
// consumption to the controller, which surfaces it here for FinOps tooling to scrape.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// AdmissionDecisions counts pod-admission governance decisions.
	AdmissionDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokencontrol_admission_decisions_total",
		Help: "Pod admission decisions made by the governance webhook, by decision and enforcement mode.",
	}, []string{"decision", "namespace", "enforcement"})

	// ModelViolations counts attempts to use a model not permitted for the scope.
	ModelViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokencontrol_model_violations_total",
		Help: "Declared model usages that are not permitted by the effective policy.",
	}, []string{"namespace", "provider", "model", "enforcement"})

	// CredentialsInjected counts credential injections performed by the mutating webhook.
	CredentialsInjected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokencontrol_credentials_injected_total",
		Help: "Credential injections performed by the mutating webhook.",
	}, []string{"namespace", "credential"})

	// CredentialSyncedNamespaces reports how many namespaces a credential is replicated into.
	CredentialSyncedNamespaces = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_credential_synced_namespaces",
		Help: "Number of namespaces a ModelCredential's Secret is currently synced into.",
	}, []string{"credential"})

	// CredentialCapacityTPM reports a credential's declared supply (tokens-per-minute capacity).
	CredentialCapacityTPM = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_credential_capacity_tokens_per_minute",
		Help: "Declared per-minute token capacity (supply) of a ModelCredential's key; 0 when unset.",
	}, []string{"credential"})

	// CredentialAllocatedTPM reports the summed per-minute token budget committed against a credential.
	CredentialAllocatedTPM = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_credential_allocated_tokens_per_minute",
		Help: "Summed per-minute token budget of policies that bind a ModelCredential (planning demand).",
	}, []string{"credential"})

	// CredentialOversubscribed is 1 when a credential's allocated demand exceeds its capacity.
	CredentialOversubscribed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_credential_oversubscribed",
		Help: "1 when a ModelCredential's allocated budgets exceed its declared capacity, else 0.",
	}, []string{"credential"})

	// EffectiveModels reports the number of permitted models per policy.
	EffectiveModels = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_effective_models",
		Help: "Number of permitted (Allow) models resolved for a policy.",
	}, []string{"namespace", "policy"})

	// ModelClaimAllowedModels reports the number of a ModelClaim's requested models that are
	// permitted by the effective policy (the claim's bindable surface).
	ModelClaimAllowedModels = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_modelclaim_allowed_models",
		Help: "Number of a ModelClaim's requested models permitted by the effective policy.",
	}, []string{"namespace", "claim"})

	// GatewayArtifacts reports the number of generated gateway artifacts managed per policy.
	GatewayArtifacts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokencontrol_gateway_artifacts",
		Help: "Generated downstream gateway artifacts currently managed, by type.",
	}, []string{"namespace", "type"})

	// TokensConsumed is an advisory counter of tokens consumed, fed by an external reporter.
	TokensConsumed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokencontrol_tokens_consumed_total",
		Help: "Advisory count of tokens consumed per scope, fed by an external usage reporter.",
	}, []string{"namespace", "workload", "provider", "model"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		AdmissionDecisions,
		ModelViolations,
		CredentialsInjected,
		CredentialSyncedNamespaces,
		CredentialCapacityTPM,
		CredentialAllocatedTPM,
		CredentialOversubscribed,
		EffectiveModels,
		GatewayArtifacts,
		TokensConsumed,
		ModelClaimAllowedModels,
	)
}
