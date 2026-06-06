package webhookv1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/token-control/token-control/api/v1alpha1"
)

func TestModelClaimValidatorRejectsBadSelector(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	v := &ModelClaimValidator{Client: c}
	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "payments"},
		Spec: api.ModelClaimSpec{
			Selector: &api.WorkloadSelector{PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "BadOp"}},
			}},
			Models: []api.ModelRequest{{Provider: "openai", Model: "gpt-4o-mini"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), mcl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}

func TestModelClaimValidatorWarnsOnUnpermittedModel(t *testing.T) {
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Models: []api.ModelPermission{{Provider: "openai", Model: "gpt-4o-mini", Action: api.ActionAllow}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ctp, ns).Build()
	v := &ModelClaimValidator{Client: c}

	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "payments"},
		Spec: api.ModelClaimSpec{
			Models: []api.ModelRequest{{Provider: "openai", Model: "gpt-4"}}, // not on allowlist
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), mcl)
	require.NoError(t, err, "an unpermitted model is a soft warning, not a hard error")
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0], "will not bind")
}

func TestModelClaimValidatorWarnsOnMissingCredential(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ns).Build()
	v := &ModelClaimValidator{Client: c}

	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "payments"},
		Spec: api.ModelClaimSpec{
			Models: []api.ModelRequest{{
				Provider: "openai", Model: "gpt-4o-mini",
				CredentialRef: &api.CredentialReference{Name: "ghost"},
			}},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), mcl)
	require.NoError(t, err)
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0], "ghost")
}

func TestModelClaimValidatorAllowsPermittedModel(t *testing.T) {
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Models: []api.ModelPermission{{Provider: "openai", Model: "gpt-4o-*", Action: api.ActionAllow}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ctp, ns).Build()
	v := &ModelClaimValidator{Client: c}

	mcl := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "payments"},
		Spec: api.ModelClaimSpec{
			Models: []api.ModelRequest{{Provider: "openai", Model: "gpt-4o-mini"}},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), mcl)
	require.NoError(t, err)
	assert.Empty(t, warnings, "a permitted model produces no warnings")
}
