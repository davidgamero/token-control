// Package gateway translates a resolved TokenQuota into downstream gateway artifacts.
//
// token-control deliberately stays out of the request hot path. When a policy opts into a
// gateway integration, the controller *generates* the native rate-limit object for that
// gateway (Envoy Gateway BackendTrafficPolicy, Kuadrant TokenRateLimitPolicy) and lets the
// existing data plane enforce token budgets at request time. The objects are produced as
// unstructured so token-control takes no compile-time dependency on those projects' APIs;
// they are only applied when the corresponding CRDs are installed in the cluster.
package gateway

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	api "github.com/token-control/token-control/api/v1alpha1"
)

// GVKs of the supported downstream artifacts.
var (
	EnvoyBackendTrafficPolicyGVK = schema.GroupVersionKind{
		Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "BackendTrafficPolicy",
	}
	KuadrantTokenRateLimitPolicyGVK = schema.GroupVersionKind{
		Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy",
	}
)

// ArtifactName is the deterministic name of the generated object for a policy.
func ArtifactName(policyName string) string {
	return "tc-" + policyName
}

// For returns the unstructured artifact for the requested gateway type, or nil for None.
func For(gw *api.GatewayIntegration, tp *api.TokenPolicy, quota *api.TokenQuota) (*unstructured.Unstructured, error) {
	if gw == nil || gw.Type == api.GatewayNone || gw.Type == "" {
		return nil, nil
	}
	if gw.TargetRef == nil {
		return nil, fmt.Errorf("gateway integration %q requires spec.gateway.targetRef", gw.Type)
	}
	switch gw.Type {
	case api.GatewayEnvoyAIGateway:
		return buildEnvoyBackendTrafficPolicy(gw, tp, quota), nil
	case api.GatewayKuadrant:
		return buildKuadrantTokenRateLimitPolicy(gw, tp, quota), nil
	default:
		return nil, fmt.Errorf("unsupported gateway type %q", gw.Type)
	}
}

func baseMeta(gvk schema.GroupVersionKind, tp *api.TokenPolicy) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(ArtifactName(tp.Name))
	u.SetNamespace(tp.Namespace)
	u.SetLabels(map[string]string{
		api.LabelManagedBy:    api.ManagedByValue,
		"governance.tokencontrol.io/policy": tp.Name,
	})
	return u
}

func targetRef(t *api.GatewayTargetRef) map[string]interface{} {
	m := map[string]interface{}{"kind": t.Kind, "name": t.Name}
	if t.Group != "" {
		m["group"] = t.Group
	}
	return m
}

// buildEnvoyBackendTrafficPolicy emits an Envoy Gateway BackendTrafficPolicy with a global
// rate limit derived from the quota. The request-per-minute limit is taken directly; the
// token budgets are recorded as annotations and as a cost-based descriptor so Envoy AI
// Gateway's usage-based rate limiting can consume them.
func buildEnvoyBackendTrafficPolicy(gw *api.GatewayIntegration, tp *api.TokenPolicy, quota *api.TokenQuota) *unstructured.Unstructured {
	u := baseMeta(EnvoyBackendTrafficPolicyGVK, tp)

	rules := []interface{}{}
	if quota != nil && quota.RequestsPerMinute != nil {
		rules = append(rules, map[string]interface{}{
			"limit": map[string]interface{}{
				"requests": *quota.RequestsPerMinute,
				"unit":     "Minute",
			},
		})
	}
	if quota != nil && quota.TokensPerMinute != nil {
		// Cost-based rule: charge per response token against a per-minute token budget.
		rules = append(rules, map[string]interface{}{
			"limit": map[string]interface{}{
				"requests": *quota.TokensPerMinute,
				"unit":     "Minute",
			},
			"cost": map[string]interface{}{
				"response": map[string]interface{}{
					"metadata": map[string]interface{}{
						"namespace": "io.envoy.ai_gateway",
						"key":       "llm_total_token",
					},
				},
			},
		})
	}
	if len(rules) == 0 {
		// Always emit at least an empty global rate limit so the object is valid/observable.
		rules = append(rules, map[string]interface{}{
			"limit": map[string]interface{}{"requests": int64(0), "unit": "Minute"},
		})
	}

	u.Object["spec"] = map[string]interface{}{
		"targetRefs": []interface{}{targetRef(gw.TargetRef)},
		"rateLimit": map[string]interface{}{
			"type": "Global",
			"global": map[string]interface{}{
				"rules": rules,
			},
		},
	}
	annotateQuota(u, quota)
	return u
}

// buildKuadrantTokenRateLimitPolicy emits a Kuadrant TokenRateLimitPolicy targeting the
// referenced Gateway/HTTPRoute with the token budgets expressed as rates.
func buildKuadrantTokenRateLimitPolicy(gw *api.GatewayIntegration, tp *api.TokenPolicy, quota *api.TokenQuota) *unstructured.Unstructured {
	u := baseMeta(KuadrantTokenRateLimitPolicyGVK, tp)

	rates := []interface{}{}
	if quota != nil {
		if quota.TokensPerMinute != nil {
			rates = append(rates, map[string]interface{}{"limit": *quota.TokensPerMinute, "window": "1m"})
		}
		if quota.TokensPerDay != nil {
			rates = append(rates, map[string]interface{}{"limit": *quota.TokensPerDay, "window": "24h"})
		}
	}
	if len(rates) == 0 {
		rates = append(rates, map[string]interface{}{"limit": int64(0), "window": "1m"})
	}

	tref := targetRef(gw.TargetRef)
	if _, ok := tref["group"]; !ok {
		tref["group"] = "gateway.networking.k8s.io"
	}
	u.Object["spec"] = map[string]interface{}{
		"targetRef": tref,
		"limits": map[string]interface{}{
			"tokens": map[string]interface{}{
				"rates": rates,
			},
		},
	}
	annotateQuota(u, quota)
	return u
}

func annotateQuota(u *unstructured.Unstructured, quota *api.TokenQuota) {
	ann := map[string]string{}
	if quota != nil {
		if quota.TokensPerMinute != nil {
			ann["governance.tokencontrol.io/tpm"] = fmt.Sprintf("%d", *quota.TokensPerMinute)
		}
		if quota.TokensPerDay != nil {
			ann["governance.tokencontrol.io/tokens-per-day"] = fmt.Sprintf("%d", *quota.TokensPerDay)
		}
		if quota.TokensPerMonth != nil {
			ann["governance.tokencontrol.io/tokens-per-month"] = fmt.Sprintf("%d", *quota.TokensPerMonth)
		}
	}
	if len(ann) > 0 {
		u.SetAnnotations(ann)
	}
}
