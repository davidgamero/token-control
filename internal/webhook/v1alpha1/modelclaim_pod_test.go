package webhookv1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/token-control/token-control/api/v1alpha1"
)

// claimGovernedClient builds a cluster that enforces an openai/gpt-4o-* allowlist (bound to
// the "openai" credential) and contains a ModelClaim selecting the "payments-sa" service
// account for the given requested models. No pod annotation is involved.
func claimGovernedClient(t *testing.T, claimModels []api.ModelRequest, credBound bool) client.Client {
	t.Helper()
	allow := api.ModelPermission{Provider: "openai", Model: "gpt-4o-*", Action: api.ActionAllow}
	if credBound {
		allow.CredentialRef = &api.CredentialReference{Name: "openai"}
	}
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Enforcement: api.EnforcementEnforce,
			Models:      []api.ModelPermission{allow},
		},
	}
	cred := &api.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Name: "openai"},
		Spec: api.ModelCredentialSpec{
			Provider:          "openai",
			SecretRef:         api.SecretKeySelector{Name: "openai-key", Namespace: "token-control-system", Key: "apiKey"},
			AllowedNamespaces: []string{"payments"},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	claim := &api.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "payments"},
		Spec: api.ModelClaimSpec{
			Selector: &api.WorkloadSelector{ServiceAccounts: []string{"payments-sa"}},
			Models:   claimModels,
		},
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ctp, cred, ns, claim).Build()
}

// claimPod is a pod with no model annotation; its model governance comes solely from a
// ModelClaim that selects its service account.
func claimPod(sa string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "payments"},
		Spec: corev1.PodSpec{
			ServiceAccountName: sa,
			Containers:         []corev1.Container{{Name: "app", Image: "app:latest"}},
		},
	}
}

func TestPodValidatorGatesOnModelClaimWithoutAnnotation(t *testing.T) {
	// The claim requests a model that is NOT on the allowlist: the pod must be denied even
	// though it carries no annotation -- governance follows workload identity.
	c := claimGovernedClient(t, []api.ModelRequest{{Provider: "openai", Model: "gpt-4"}}, true)
	v := &PodValidator{Client: c, Config: Config{OperatorNamespace: "token-control-system"}}
	_, err := v.ValidateCreate(context.Background(), claimPod("payments-sa"))
	require.Error(t, err, "claim-declared unlisted model must be denied")
	assert.Contains(t, err.Error(), "allowlist")
}

func TestPodValidatorAllowsClaimPermittedModel(t *testing.T) {
	c := claimGovernedClient(t, []api.ModelRequest{{Provider: "openai", Model: "gpt-4o-mini"}}, true)
	v := &PodValidator{Client: c, Config: Config{OperatorNamespace: "token-control-system"}}
	_, err := v.ValidateCreate(context.Background(), claimPod("payments-sa"))
	assert.NoError(t, err)
}

func TestPodValidatorIgnoresNonMatchingClaim(t *testing.T) {
	// A pod whose identity the claim does not select declares nothing and is not gated.
	c := claimGovernedClient(t, []api.ModelRequest{{Provider: "openai", Model: "gpt-4"}}, true)
	v := &PodValidator{Client: c, Config: Config{OperatorNamespace: "token-control-system"}}
	_, err := v.ValidateCreate(context.Background(), claimPod("other-sa"))
	assert.NoError(t, err, "claim selects payments-sa only; other-sa declares no models")
}

func TestPodDefaulterInjectsCredentialFromClaim(t *testing.T) {
	c := claimGovernedClient(t, []api.ModelRequest{{Provider: "openai", Model: "gpt-4o-mini"}}, true)
	d := &PodDefaulter{Client: c, Config: Config{OperatorNamespace: "token-control-system"}}
	pod := claimPod("payments-sa")
	require.NoError(t, d.Default(context.Background(), pod))

	require.Len(t, pod.Spec.Containers[0].Env, 1, "credential bound by the cluster policy is injected")
	assert.Equal(t, "OPENAI_API_KEY", pod.Spec.Containers[0].Env[0].Name)
	assert.Equal(t, "openai", pod.Annotations[api.AnnotationCredentialsBound])
}

func TestPodDefaulterUsesClaimCredentialFallback(t *testing.T) {
	// The allowlist permits the model but binds NO credential; the claim's own credentialRef
	// supplies the injection fallback.
	c := claimGovernedClient(t, []api.ModelRequest{{
		Provider: "openai", Model: "gpt-4o-mini",
		CredentialRef: &api.CredentialReference{Name: "openai"},
	}}, false)
	d := &PodDefaulter{Client: c, Config: Config{OperatorNamespace: "token-control-system"}}
	pod := claimPod("payments-sa")
	require.NoError(t, d.Default(context.Background(), pod))

	require.Len(t, pod.Spec.Containers[0].Env, 1, "claim credentialRef is the injection fallback")
	assert.Equal(t, "OPENAI_API_KEY", pod.Spec.Containers[0].Env[0].Name)
	assert.Equal(t, "openai", pod.Annotations[api.AnnotationCredentialsBound])
}
