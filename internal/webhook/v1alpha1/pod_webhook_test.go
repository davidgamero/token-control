package webhookv1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/token-control/token-control/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, api.AddToScheme(s))
	return s
}

func governedClient(t *testing.T) client.Client {
	t.Helper()
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Enforcement: api.EnforcementEnforce,
			Models: []api.ModelPermission{{
				Provider: "openai", Model: "gpt-4o-*", Action: api.ActionAllow,
				CredentialRef: &api.CredentialReference{Name: "openai"},
			}},
		},
	}
	mc := &api.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Name: "openai"},
		Spec: api.ModelCredentialSpec{
			Provider:          "openai",
			SecretRef:         api.SecretKeySelector{Name: "openai-key", Namespace: "token-control-system", Key: "apiKey"},
			AllowedNamespaces: []string{"payments"},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ctp, mc, ns).Build()
}

func podWithModels(models string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker",
			Namespace:   "payments",
			Annotations: map[string]string{api.AnnotationModels: models},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:latest"}},
		},
	}
}

func TestPodValidatorAllowsPermittedModel(t *testing.T) {
	v := &PodValidator{Client: governedClient(t), Config: Config{OperatorNamespace: "token-control-system"}}
	_, err := v.ValidateCreate(context.Background(), podWithModels("openai/gpt-4o-mini"))
	assert.NoError(t, err)
}

func TestPodValidatorDeniesUnlistedModelInEnforce(t *testing.T) {
	v := &PodValidator{Client: governedClient(t), Config: Config{OperatorNamespace: "token-control-system"}}
	_, err := v.ValidateCreate(context.Background(), podWithModels("openai/gpt-4"))
	require.Error(t, err, "gpt-4 is not on the allowlist and must be denied")
	assert.Contains(t, err.Error(), "allowlist")
}

func TestPodValidatorExemptNamespacePasses(t *testing.T) {
	v := &PodValidator{Client: governedClient(t), Config: Config{OperatorNamespace: "token-control-system"}}
	pod := podWithModels("openai/gpt-4")
	pod.Namespace = "kube-system"
	v.Config.ExemptNamespaces = map[string]struct{}{"kube-system": {}}
	_, err := v.ValidateCreate(context.Background(), pod)
	assert.NoError(t, err, "exempt namespaces are not governed")
}

func TestPodValidatorAuditModeWarnsButAllows(t *testing.T) {
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Enforcement: api.EnforcementAudit,
			Models:      []api.ModelPermission{{Provider: "openai", Model: "gpt-4o-mini", Action: api.ActionAllow}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ctp, ns).Build()
	v := &PodValidator{Client: c, Config: Config{OperatorNamespace: "token-control-system"}}
	warnings, err := v.ValidateCreate(context.Background(), podWithModels("openai/gpt-4"))
	assert.NoError(t, err, "audit mode must admit")
	assert.NotEmpty(t, warnings, "audit mode must warn")
}

func TestPodDefaulterInjectsCredentialEnv(t *testing.T) {
	d := &PodDefaulter{Client: governedClient(t), Config: Config{OperatorNamespace: "token-control-system"}}
	pod := podWithModels("openai/gpt-4o-mini")
	require.NoError(t, d.Default(context.Background(), pod))

	require.Len(t, pod.Spec.Containers, 1)
	env := pod.Spec.Containers[0].Env
	require.Len(t, env, 1)
	assert.Equal(t, "OPENAI_API_KEY", env[0].Name)
	require.NotNil(t, env[0].ValueFrom)
	require.NotNil(t, env[0].ValueFrom.SecretKeyRef)
	assert.Equal(t, "tc-cred-openai", env[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "apiKey", env[0].ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, "openai", pod.Annotations[api.AnnotationCredentialsBound])
}

func TestPodDefaulterSkipsUnauthorizedNamespace(t *testing.T) {
	d := &PodDefaulter{Client: governedClient(t), Config: Config{OperatorNamespace: "token-control-system"}}
	pod := podWithModels("openai/gpt-4o-mini")
	pod.Namespace = "analytics" // not in the credential's AllowedNamespaces
	// Resolver still governs via the cluster policy, but the credential must not be injected.
	require.NoError(t, d.Default(context.Background(), pod))
	assert.Empty(t, pod.Spec.Containers[0].Env, "credential must not be injected for unauthorized namespace")
}

func TestPodDefaulterRespectsOptOut(t *testing.T) {
	d := &PodDefaulter{Client: governedClient(t), Config: Config{OperatorNamespace: "token-control-system"}}
	pod := podWithModels("openai/gpt-4o-mini")
	pod.Annotations[api.AnnotationInjectionDisabled] = "true"
	require.NoError(t, d.Default(context.Background(), pod))
	assert.Empty(t, pod.Spec.Containers[0].Env)
}

func TestTokenPolicyValidatorWarnsOnWidening(t *testing.T) {
	// Cluster permits only gpt-4o-mini; a namespace policy tries to allow gpt-4.
	ctp := &api.ClusterTokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: api.ClusterTokenPolicySpec{
			Models: []api.ModelPermission{{Provider: "openai", Model: "gpt-4o-mini", Action: api.ActionAllow}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ctp, ns).Build()
	v := &TokenPolicyValidator{Client: c}

	tp := &api.TokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-default", Namespace: "payments"},
		Spec: api.TokenPolicySpec{
			Models: []api.ModelPermission{{Provider: "openai", Model: "gpt-4", Action: api.ActionAllow}},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), tp)
	require.NoError(t, err, "widening is a warning, not a hard error (the resolver still narrows)")
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0], "no effect")
}

func TestTokenPolicyValidatorRejectsBadSelector(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	v := &TokenPolicyValidator{Client: c}
	tp := &api.TokenPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "payments"},
		Spec: api.TokenPolicySpec{
			Selector: &api.WorkloadSelector{PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "BadOp"}},
			}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), tp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}
