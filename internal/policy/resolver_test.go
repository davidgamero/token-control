package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/token-control/token-control/api/v1alpha1"
)

func i64(v int64) *int64 { return &v }

func allow(provider, model string) api.ModelPermission {
	return api.ModelPermission{Provider: provider, Model: model, Action: api.ActionAllow}
}

func deny(provider, model string) api.ModelPermission {
	return api.ModelPermission{Provider: provider, Model: model, Action: api.ActionDeny}
}

func clusterPolicy(name string, sel *metav1.LabelSelector, models ...api.ModelPermission) api.ClusterTokenPolicy {
	return api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: api.ClusterTokenPolicySpec{
			NamespaceSelector: sel,
			Models:            models,
			Enforcement:       api.EnforcementEnforce,
		},
	}
}

func nsPolicy(ns, name string, sel *api.WorkloadSelector, models ...api.ModelPermission) api.TokenPolicy {
	return api.TokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       api.TokenPolicySpec{Selector: sel, Models: models},
	}
}

func TestUngovernedNamespaceAllowsEverything(t *testing.T) {
	eff, err := Resolve(ResolveInput{Namespace: "payments"})
	require.NoError(t, err)
	assert.False(t, eff.Governed)
	assert.False(t, eff.ModelGoverned)
	d := eff.Permit("openai", "gpt-4")
	assert.True(t, d.Allowed, "ungoverned namespace should permit any model")
}

func TestClusterAllowlistDeniesUnlisted(t *testing.T) {
	in := ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{clusterPolicy("default", nil, allow("openai", "gpt-4o-mini"))},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	assert.True(t, eff.Governed)
	assert.True(t, eff.ModelGoverned)

	assert.True(t, eff.Permit("openai", "gpt-4o-mini").Allowed)
	d := eff.Permit("openai", "gpt-4")
	assert.False(t, d.Allowed, "model not on cluster allowlist must be denied")
	assert.Contains(t, d.Reason, "allowlist")
}

func TestNamespaceNarrowsCluster(t *testing.T) {
	in := ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{clusterPolicy("default", nil, allow("openai", "*"))},
		NamespacePolicies: []api.TokenPolicy{
			nsPolicy("payments", "ns-default", nil, allow("openai", "gpt-4o-mini")),
		},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	// Cluster allows openai/*, but the namespace allowlist only permits gpt-4o-mini.
	assert.True(t, eff.Permit("openai", "gpt-4o-mini").Allowed)
	assert.False(t, eff.Permit("openai", "gpt-4").Allowed, "namespace allowlist must narrow cluster")
}

func TestWorkloadCannotWidenNamespace(t *testing.T) {
	// Namespace only permits gpt-4o-mini. A workload policy tries to allow gpt-4.
	in := ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{clusterPolicy("default", nil, allow("openai", "*"))},
		NamespacePolicies: []api.TokenPolicy{
			nsPolicy("payments", "ns-default", nil, allow("openai", "gpt-4o-mini")),
			nsPolicy("payments", "wl", &api.WorkloadSelector{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
			}, allow("openai", "gpt-4")),
		},
		PodLabels: map[string]string{"app": "x"},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	// The workload "allowed" gpt-4, but the namespace tier denies-by-omission, so it stays denied.
	assert.False(t, eff.Permit("openai", "gpt-4").Allowed, "workload must not widen beyond namespace")
	// gpt-4o-mini is allowed by namespace but the workload allowlist omits it -> denied by workload.
	assert.False(t, eff.Permit("openai", "gpt-4o-mini").Allowed, "workload allowlist omits the model")
}

func TestDenyAlwaysWins(t *testing.T) {
	in := ResolveInput{
		Namespace: "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{
			clusterPolicy("default", nil, allow("openai", "*"), deny("openai", "gpt-4")),
		},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	assert.True(t, eff.Permit("openai", "gpt-4o").Allowed)
	d := eff.Permit("openai", "gpt-4")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "denied")
}

func TestGlobMatching(t *testing.T) {
	in := ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{clusterPolicy("default", nil, allow("openai", "gpt-4o-*"))},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	assert.True(t, eff.Permit("openai", "gpt-4o-mini").Allowed)
	assert.True(t, eff.Permit("OpenAI", "GPT-4o-MINI").Allowed, "matching is case-insensitive")
	assert.False(t, eff.Permit("openai", "gpt-4").Allowed)
	assert.False(t, eff.Permit("anthropic", "gpt-4o-mini").Allowed, "provider must also match")
}

func TestQuotaIsMostRestrictive(t *testing.T) {
	ctp := clusterPolicy("default", nil, allow("openai", "*"))
	ctp.Spec.Quota = &api.TokenQuota{TokensPerMinute: i64(100000), TokensPerDay: i64(5000000)}
	tp := nsPolicy("payments", "ns-default", nil, allow("openai", "*"))
	tp.Spec.Quota = &api.TokenQuota{TokensPerMinute: i64(20000)} // tighter TPM, no day limit

	eff, err := Resolve(ResolveInput{
		Namespace:         "payments",
		ClusterPolicies:   []api.ClusterTokenPolicy{ctp},
		NamespacePolicies: []api.TokenPolicy{tp},
	})
	require.NoError(t, err)
	require.NotNil(t, eff.Quota)
	require.NotNil(t, eff.Quota.TokensPerMinute)
	assert.Equal(t, int64(20000), *eff.Quota.TokensPerMinute, "TPM should be the smaller of the two")
	require.NotNil(t, eff.Quota.TokensPerDay)
	assert.Equal(t, int64(5000000), *eff.Quota.TokensPerDay, "day limit inherited from cluster")
}

func TestCredentialMostSpecificWins(t *testing.T) {
	ctp := clusterPolicy("default", nil)
	ctp.Spec.Models = []api.ModelPermission{{
		Provider: "openai", Model: "*", Action: api.ActionAllow,
		CredentialRef: &api.CredentialReference{Name: "openai-cluster"},
	}}
	tp := nsPolicy("payments", "ns-default", nil)
	tp.Spec.Models = []api.ModelPermission{{
		Provider: "openai", Model: "gpt-4o-mini", Action: api.ActionAllow,
		CredentialRef: &api.CredentialReference{Name: "openai-payments"},
	}}

	eff, err := Resolve(ResolveInput{
		Namespace:         "payments",
		ClusterPolicies:   []api.ClusterTokenPolicy{ctp},
		NamespacePolicies: []api.TokenPolicy{tp},
	})
	require.NoError(t, err)
	d := eff.Permit("openai", "gpt-4o-mini")
	require.True(t, d.Allowed)
	assert.Equal(t, "openai-payments", d.Credential, "namespace credential is more specific than cluster")
}

func TestDefaultCredentialFallback(t *testing.T) {
	ctp := clusterPolicy("default", nil, allow("openai", "*"))
	ctp.Spec.DefaultCredentialRef = &api.CredentialReference{Name: "openai-default"}
	eff, err := Resolve(ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{ctp},
	})
	require.NoError(t, err)
	d := eff.Permit("openai", "gpt-4")
	require.True(t, d.Allowed)
	assert.Equal(t, "openai-default", d.Credential)
}

func TestNamespaceSelectorScopesClusterPolicy(t *testing.T) {
	ctp := clusterPolicy("tenants", &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "tenant"}}, allow("openai", "gpt-4o-mini"))
	// Namespace without the label is not governed by the cluster policy.
	eff, err := Resolve(ResolveInput{
		Namespace:       "infra",
		NamespaceLabels: map[string]string{"tier": "infra"},
		ClusterPolicies: []api.ClusterTokenPolicy{ctp},
	})
	require.NoError(t, err)
	assert.False(t, eff.Governed)
	assert.True(t, eff.Permit("openai", "gpt-4").Allowed, "non-selected namespace is ungoverned")

	// Namespace with the label is governed.
	eff2, err := Resolve(ResolveInput{
		Namespace:       "tenant-a",
		NamespaceLabels: map[string]string{"tier": "tenant"},
		ClusterPolicies: []api.ClusterTokenPolicy{ctp},
	})
	require.NoError(t, err)
	assert.True(t, eff2.Governed)
	assert.False(t, eff2.Permit("openai", "gpt-4").Allowed)
}

func TestServiceAccountSelector(t *testing.T) {
	in := ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{clusterPolicy("default", nil, allow("openai", "*"))},
		NamespacePolicies: []api.TokenPolicy{
			nsPolicy("payments", "batch", &api.WorkloadSelector{ServiceAccounts: []string{"batch-runner"}}, allow("openai", "gpt-4o-mini")),
		},
		ServiceAccount: "batch-runner",
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	assert.False(t, eff.Permit("openai", "gpt-4").Allowed, "matched SA workload policy narrows to mini")

	// A different SA is not matched by the workload policy, so only the cluster tier applies.
	in.ServiceAccount = "api-server"
	eff2, err := Resolve(in)
	require.NoError(t, err)
	assert.True(t, eff2.Permit("openai", "gpt-4").Allowed)
}

func TestEffectiveModelsEnumeration(t *testing.T) {
	in := ResolveInput{
		Namespace: "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{
			clusterPolicy("default", nil, allow("openai", "gpt-4o-mini"), allow("anthropic", "claude-3-5-sonnet")),
		},
		NamespacePolicies: []api.TokenPolicy{
			nsPolicy("payments", "ns", nil, allow("openai", "gpt-4o-mini")), // narrows: drops anthropic
		},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	models := eff.EffectiveModels()
	// Both Allow rules are enumerated, but anthropic is denied-by-omission at the namespace tier.
	byKey := map[string]api.EffectiveModel{}
	for _, m := range models {
		byKey[m.Provider+"/"+m.Model] = m
	}
	require.Contains(t, byKey, "openai/gpt-4o-mini")
	require.Contains(t, byKey, "anthropic/claude-3-5-sonnet")
	assert.Equal(t, api.ActionAllow, byKey["openai/gpt-4o-mini"].Action)
	assert.Equal(t, api.ActionDeny, byKey["anthropic/claude-3-5-sonnet"].Action)
}

func TestPureDenylistTier(t *testing.T) {
	// Cluster allows everything; namespace is a denylist that only blocks gpt-4.
	in := ResolveInput{
		Namespace:       "payments",
		ClusterPolicies: []api.ClusterTokenPolicy{clusterPolicy("default", nil, allow("openai", "*"))},
		NamespacePolicies: []api.TokenPolicy{
			nsPolicy("payments", "ns", nil, deny("openai", "gpt-4")),
		},
	}
	eff, err := Resolve(in)
	require.NoError(t, err)
	assert.True(t, eff.Permit("openai", "gpt-4o").Allowed, "denylist permits unlisted models")
	assert.False(t, eff.Permit("openai", "gpt-4").Allowed)
}
