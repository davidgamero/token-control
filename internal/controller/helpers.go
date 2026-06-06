// Package controller contains the token-control reconcilers. The controllers never sit in
// the request hot path: they resolve effective policy into status, replicate credentials
// into authorized namespaces, and (optionally) generate downstream gateway rate-limit
// artifacts. Request-time enforcement is handled by the admission webhooks and, when
// configured, by the gateway the controller generates configuration for.
package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	api "github.com/token-control/token-control/api/v1alpha1"
)

func setCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, msg string, gen int64) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gen,
	})
}

// distinctAllowedCredentials returns the unique, ordered credential names from a resolved
// effective-model list, considering only permitted (Allow) entries.
func distinctAllowedCredentials(models []api.EffectiveModel) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range models {
		if m.Action != api.ActionAllow || m.Credential == "" {
			continue
		}
		if !seen[m.Credential] {
			seen[m.Credential] = true
			out = append(out, m.Credential)
		}
	}
	return out
}

func countAllowed(models []api.EffectiveModel) float64 {
	n := 0.0
	for _, m := range models {
		if m.Action == api.ActionAllow {
			n++
		}
	}
	return n
}
