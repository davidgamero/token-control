package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/token-control/token-control/api/v1alpha1"
)

func ctlScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, api.AddToScheme(s))
	return s
}

func reconcileClaim(t *testing.T, c client.Client, mcl *api.ModelClaim) *api.ModelClaim {
	t.Helper()
	r := &ModelClaimReconciler{Client: c, Scheme: ctlScheme(t)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mcl)})
	require.NoError(t, err)
	var out api.ModelClaim
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(mcl), &out))
	return &out
}

func TestModelClaimBindsWhenAllModelsPermitted(t *testing.T) {
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Models: []api.ModelPermission{{
				Provider: "openai", Model: "gpt-4o-*", Action: api.ActionAllow,
				CredentialRef: &api.CredentialReference{Name: "openai"},
			}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "team-claim", Namespace: "payments", Generation: 1},
		Spec: api.ModelClaimSpec{
			Models: []api.ModelRequest{{Provider: "openai", Model: "gpt-4o-mini"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(ctlScheme(t)).
		WithObjects(ctp, ns, mcl).
		WithStatusSubresource(&api.ModelClaim{}).
		Build()

	out := reconcileClaim(t, c, mcl)
	assert.Equal(t, api.ClaimBound, out.Status.Phase)
	require.Len(t, out.Status.ResolvedModels, 1)
	assert.Equal(t, api.ActionAllow, out.Status.ResolvedModels[0].Action)
	assert.Equal(t, "openai", out.Status.ResolvedModels[0].Credential, "credential bound from the cluster policy")
	assert.Equal(t, []string{"openai"}, out.Status.BoundCredentials)
	assert.Equal(t, int64(1), out.Status.ObservedGeneration)

	cond := findCond(out.Status.Conditions, api.ConditionBound)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestModelClaimDeniedWhenModelNotPermitted(t *testing.T) {
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Models: []api.ModelPermission{{Provider: "openai", Model: "gpt-4o-mini", Action: api.ActionAllow}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "team-claim", Namespace: "payments", Generation: 1},
		Spec: api.ModelClaimSpec{
			Models: []api.ModelRequest{
				{Provider: "openai", Model: "gpt-4o-mini"}, // allowed
				{Provider: "openai", Model: "gpt-4"},       // not on allowlist
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(ctlScheme(t)).
		WithObjects(ctp, ns, mcl).
		WithStatusSubresource(&api.ModelClaim{}).
		Build()

	out := reconcileClaim(t, c, mcl)
	assert.Equal(t, api.ClaimDenied, out.Status.Phase)
	require.Len(t, out.Status.ResolvedModels, 2)

	deny := out.Status.ResolvedModels[1]
	assert.Equal(t, "gpt-4", deny.Model)
	assert.Equal(t, api.ActionDeny, deny.Action)

	cond := findCond(out.Status.Conditions, api.ConditionBound)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}

func TestModelClaimBoundInUngovernedNamespace(t *testing.T) {
	// No policy configures a model allowlist, so any requested model is permitted.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sandbox"}}
	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "sandbox", Generation: 1},
		Spec: api.ModelClaimSpec{
			Models: []api.ModelRequest{{Provider: "anthropic", Model: "claude-3-5-sonnet"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(ctlScheme(t)).
		WithObjects(ns, mcl).
		WithStatusSubresource(&api.ModelClaim{}).
		Build()

	out := reconcileClaim(t, c, mcl)
	assert.Equal(t, api.ClaimBound, out.Status.Phase, "no allowlist => trivially bound")
}

func findCond(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
