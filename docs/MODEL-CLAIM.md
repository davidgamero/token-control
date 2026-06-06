# Replacing the model annotation with a validated claim

This note evaluates how to replace the self-asserted `governance.tokencontrol.io/models`
pod annotation with a strongly-validated, config-rich mechanism — a CRD or Kubernetes
Dynamic Resource Allocation (DRA: `ResourceSlice` / `ResourceClaim`). It is a design
discussion, not a committed roadmap.

## Why the annotation is weak

Today a workload declares intent with a free-form annotation that the webhook parses by hand
(`internal/webhook/v1alpha1/webhook_common.go:46`, `parseModels`). Its weaknesses:

- **Opaque string, no schema.** `"openai/gpt-4o,anthropic/claude-3"` is split on commas and
  slashes; there is no OpenAPI validation, no typing, and one malformed token is silently
  dropped (`webhook_common.go:57-60`).
- **No room for config.** A single annotation can't carry credential preference, a budget
  request, a purpose/audit string, or per-model parameters.
- **No feedback.** The author learns nothing until a pod is admitted or rejected; there is no
  object with a `status`.
- **Pod-authored.** The deepest problem: the declaration lives on the pod, so it can be
  omitted or fabricated by whoever writes the pod template.

## Reframe: two separate problems

Replacing the annotation addresses **declaration integrity** (how intent is expressed and
validated). It is distinct from **runtime enforcement** (the trust gap in
[`ENFORCEMENT.md`](ENFORCEMENT.md)). A richer claim object does not, by itself, stop a pod that
declares nothing and simply makes HTTP calls — that still requires the egress leash (Layer 1).
The two changes are complementary: a validated claim gates *credential injection*; the
NetworkPolicy makes the *runtime* path enforce it.

## What the three primitives actually are

- **CRD** — a token-control object (proposed `ModelClaim`) selected by workload identity.
  Portable, no cluster-version floor, reuses the existing resolver and webhook machinery.
- **ResourceSlice** — DRA **supply**, published by a *driver*; never referenced from a pod.
  Adopting it means token-control advertises model endpoints as devices. It is the supply
  analogue of the `ModelCredential.spec.capacity` field.
- **ResourceClaim** — DRA **demand**, a workload's request, allocated by a driver. Requires
  token-control to ship a DRA driver.

So the real choice is **(A) our own CRD** versus **(B) full DRA**.

## Option A — a `ModelClaim` CRD (recommended)

The clean framing is requests-vs-limits / PVC-vs-PV:

- `ClusterTokenPolicy` / `TokenPolicy` = the **grant** (what an admin allows) — top-down.
- `ModelClaim` = the **claim** (what a workload asks for) — bottom-up, namespaced, RBAC'd.
- The controller **binds** the claim iff the grant permits it; `status` reflects the verdict.

Bind it by **workload identity** (reuse the existing `WorkloadSelector`: `serviceAccounts` +
`podSelector`) rather than a pod field. Then *the pod authors nothing* and cannot
bypass-by-omission for credential injection — pod identity selects its claims exactly the way
the workload policy tier already works.

```yaml
apiVersion: governance.tokencontrol.io/v1alpha1
kind: ModelClaim
metadata: { name: chatbot, namespace: team-a }
spec:
  selector:                       # which workloads this claim covers
    serviceAccounts: [chatbot]
  models:
    - provider: openai
      model: gpt-4o
  credentialRef: { name: openai } # optional preference; resolver still authorizes
  request:                        # optional declared budget (planning/audit)
    tokensPerMinute: 50000
  purpose: "support chatbot"      # free-form, for audit trails
status:
  phase: Bound                    # Bound | Denied | Pending
  resolvedModels:
    - { provider: openai, model: gpt-4o, action: Allow, credential: openai, source: "ClusterTokenPolicy/baseline" }
  conditions: [ ... ]
```

### Code deltas (surgical)

- New `ModelClaim` API type plus a **validating webhook** that runs the resolver's `Permit`
  at create time, so an unsatisfiable claim is rejected (or warned) immediately instead of
  failing silently at pod admission.
- New **reconciler** that resolves the claim against the hierarchy and writes
  `status.phase` / `resolvedModels`; it reuses `policy.Resolve` / `policy.Permit` unchanged.
- `parseModels(pod)` (`webhook_common.go:46`) becomes "list `ModelClaim`s whose selector
  matches this pod's identity"; the pod validating/mutating webhooks
  (`internal/webhook/v1alpha1/pod_webhook.go`) gate and inject off the **bound** claims. The
  existing annotation parsing can remain as a deprecated fallback during migration.

### Trade-offs

- **Pros:** strong OpenAPI + webhook validation; rich typed fields; per-claim RBAC; `status`
  feedback to the author; no cluster-version floor; reuses the existing resolver, credential
  replication and injection machinery.
- **Cons:** it is our API to own and version; binding is admission-time, not scheduler-level.

## Option B — full DRA (`ResourceSlice` + `ResourceClaim` + a driver)

token-control runs a **DRA driver**: it publishes `ResourceSlice`s describing model endpoints
as devices (provider / model / credential / capacity attributes), and workloads use a
`ResourceClaimTemplate` selecting one via CEL. The driver allocates only if policy permits and
injects the credential through the kubelet plugin's `NodePrepareResources` (CDI). Rich,
driver-validated config lives in `ResourceClaim.spec.devices.config[].opaque.parameters`.

### Trade-offs

- **Pros:** the pod **cannot be scheduled or started** until the driver allocates the claim —
  a hard gate, not merely admission-time; scheduler-native; `ResourceSlice` is the natural
  home for the capacity/supply model.
- **Cons:** heavy. A kubelet-plugin DaemonSet plus plugin registration; a cluster-version
  floor (`resource.k8s.io/v1` is GA in 1.34, `v1beta1` in 1.32); DRA is **node/device-centric**
  while a model API is a *network* resource (an awkward fit); and credential injection via CDI
  is more plumbing than the current webhook injection. A large lift for mostly a
  scheduler-integration gain over Option A.

## Honest caveats

- Neither option fully closes the runtime bypass. DRA adds a hard *scheduling* gate; the CRD
  keeps admission-time gating. Both gate **injection** on the declaration, so pairing either
  with the egress NetworkPolicy (Layer 1) is what makes the model dimension genuinely
  enforced (the gateway then sees the real request body).
- **Ecosystem alignment:** the Gateway API Inference Extension (`InferenceModel` /
  `InferencePool`) is the emerging standard for "model as a routable backend." If references
  should feel native, mirroring its naming may beat inventing `ModelClaim` wholesale.

## Recommendation

Ship **Option A (`ModelClaim`, selected by workload identity)** first: it removes the
pod-authored annotation, adds strong validation, `status` feedback and RBAC, and drops into the
existing resolver/webhook with minimal churn. Keep **DRA as an opt-in advanced mode** for later,
once scheduler-level binding and capacity-aware placement are wanted — that is where
`ResourceSlice` ties directly into the capacity work. In all cases, pair the claim with the
egress leash so the declaration is enforced at runtime, not merely validated at admission.
