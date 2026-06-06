# Enforcement: current state and the path to real enforcement

This note is an honest review of *what token-control actually enforces today* versus what
the headline features imply, and a layered plan for closing the gap. It is a design
discussion, not a committed roadmap.

## TL;DR

token-control currently **enforces only the model allowlist, at admission, against a
self-declared annotation**. Token/request budgets are enforced solely by an external gateway
— if one is installed and if traffic actually flows through it. The injected credential has
no leash, so a workload can call a provider directly and bypass every check. That single fact
is what makes the system *cooperative* rather than *enforced*.

## What enforcement does today

Two independent planes, both weaker than the docs imply:

1. **Admission gate** (the only thing token-control enforces itself) —
   `internal/webhook/v1alpha1/pod_webhook.go:69-88`. On pod create/update the validating
   webhook parses the `governance.tokencontrol.io/models` annotation, resolves the policy
   hierarchy, and rejects (`Enforce`) or warns (`Audit`) when a declared model is not allowed.
2. **Gateway config generation** — `internal/controller/tokenpolicy_controller.go:81` calls
   `gateway.For(...)` and server-side-applies a BackendTrafficPolicy / TokenRateLimitPolicy.
   The **gateway** then enforces budgets live; token-control stays off the hot path.

## The gaps

### A. The admission gate trusts a self-declared annotation
Nothing forces a pod to declare truthfully. A pod that omits the annotation, or declares
`openai/gpt-4o` but calls `openai/gpt-4-vision`, is admitted unchanged — `pod_webhook.go:52-55`
returns early when no models are declared. Model-allowlisting is therefore cooperative.

### B. Token budgets have no native enforcement
The webhook never consults `eff.Quota`; it only checks allow/deny. Quotas are enforced *only*
by a gateway, *only* when one is installed and configured. On a bare cluster the headline
"token budgets" feature does nothing.

### C. The injected credential can bypass everything
Nothing routes egress through the gateway. A workload holding the injected `OPENAI_API_KEY`
can reach `api.openai.com` directly, bypassing both model checks and token limits. There is no
NetworkPolicy generation anywhere in the codebase.

### D. ClusterTokenPolicy generates no gateway artifact
`gateway.For` takes a `*TokenPolicy`, and only `TokenPolicyReconciler` calls it.
`internal/controller/clustertokenpolicy_controller.go` updates status only. So cluster-tier
quotas never produce live enforcement unless a namespaced `TokenPolicy` also exists.

### E. Generated artifacts are global per-target, not per-workload
`internal/gateway/gateway.go:113-155` targets a Gateway/HTTPRoute with no `clientSelectors`
(Envoy) or `counters`/`when` (Kuadrant), so the "per-namespace/per-workload budget" is really
one shared bucket per target. The Envoy cost rule also references
`io.envoy.ai_gateway/llm_total_token` metadata (`gateway.go:96-103`) that is only populated if
the user separately configures an `AIGatewayRoute` with `llmRequestCosts` — which
token-control does not generate. We emit the *consumer* of that metadata but not the producer.

### F. The usage loop is never closed
`metrics.TokensConsumed` (`internal/metrics/metrics.go:70`) and `status.usage`
(`api/v1alpha1/tokenpolicy_types.go:62`) are declared but nothing ever writes them. There is
no burndown, no monthly-cap enforcement, no "90% used" — the one budget dimension a
rate-limiter handles poorly (long windows) is entirely unenforced.

## The path to real enforcement

Three independent layers, in priority order.

### Layer 1 — Close the trust gap: generate egress NetworkPolicies (highest leverage)
Make the injected key usable *only through the gateway*. When the mutating webhook injects a
credential into a governed pod, also emit a NetworkPolicy that denies egress to provider
endpoints except the in-cluster gateway Service. This collapses gaps **A** and **C**: a
workload physically cannot reach a provider directly, and the gateway — which sees the real
`model` field in the request body — becomes the source of truth for the model dimension. This
is the single change that makes everything else trustworthy, and it needs no external gateway
to be valuable on its own (it can also simply deny ungoverned LLM egress).

### Layer 2 — Make budgets real where a gateway exists
- Generate artifacts from the **cluster tier** too (fix **D**), or emit a namespace-default
  artifact.
- Add **per-workload scoping** — Kuadrant `counters`/`when`, Envoy `clientSelectors` — so
  budgets are per-workload rather than a shared global bucket (fix **E**).
- Generate the **producer** side for Envoy AI Gateway (the `AIGatewayRoute` `llmRequestCosts`)
  or document it as a prerequisite, and add **feature-detection + a dry-run apply** so schema
  mismatches surface as `GatewaySynced=False` instead of silently failing.

### Layer 3 — Close the usage loop: native, gateway-independent caps
Add a usage-ingestion path (scrape Limitador counters / Envoy dynamic metadata, or accept
pushed usage) that writes `status.usage` and `metrics.TokensConsumed`, then have the admission
webhook **additionally** deny new pods when a hard window (e.g. monthly) is already exhausted.
This is the only place token-control can enforce tokens itself, and it complements — rather
than duplicates — the gateway's short-window rate limiting.

### Not recommended: an in-cluster egress proxy
A token-control-owned egress proxy for clusters with no gateway would give full native
enforcement, but it contradicts the project's "no proxy in the hot path" principle. Treat it
as a documented escape hatch, not the default.

## Recommendation

Start with **Layer 1 (egress NetworkPolicies)**: it is self-contained, requires no external
gateway, and converts the existing admission gate and credential binding from advisory into
enforced. Follow with **Layer 3** for native long-window token enforcement that does not depend
on a gateway at all. **Layer 2** is the payoff for clusters that already run a supported
gateway.
