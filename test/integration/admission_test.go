//go:build integration

package integration

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	api "github.com/token-control/token-control/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestAdmission_PermittedModel verifies that a pod declaring a permitted model (gpt-4o) is
// admitted and receives the OPENAI_API_KEY environment variable injected by token-control.
func TestAdmission_PermittedModel(t *testing.T) {
	podName := "tc-test-permitted"
	cleanupPod(t, testNamespace, podName)
	t.Cleanup(func() { cleanupPod(t, testNamespace, podName) })

	pod := testPod(podName, map[string]string{
		"app":                                    "tc-test-caller",
		api.AnnotationModels:                     "", // claim-driven; no annotation needed
	}, "openai/gpt-4o")
	// Use the annotation path as well so the test exercises both paths.
	pod.Annotations = map[string]string{
		api.AnnotationModels: "openai/gpt-4o",
	}

	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Wait for the pod to be scheduled (Running or Succeeded means admission passed).
	if err := waitForPodPhase(testNamespace, podName, corev1.PodRunning); err != nil {
		// Also accept Pending — the container might not have an image to run, but the
		// admission webhook must have passed.
		p, getErr := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("get pod after admission: %v", getErr)
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("pod failed; expected admission to pass. phase=%s reason=%s",
				p.Status.Phase, p.Status.Reason)
		}
	}

	// Verify the credential env var was injected by the mutating webhook.
	admittedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	assertEnvVarInjected(t, admittedPod, "OPENAI_API_KEY")
	assertAnnotationContains(t, admittedPod, api.AnnotationCredentialsBound, "openai-test")
}

// TestAdmission_DeniedModel verifies that a pod declaring a denied model (claude-3-5-sonnet)
// is rejected with a Forbidden error by the validating webhook (EnforcementMode=Enforce).
func TestAdmission_DeniedModel(t *testing.T) {
	podName := "tc-test-denied"
	t.Cleanup(func() { cleanupPod(t, testNamespace, podName) })

	pod := testPod(podName, nil, "")
	pod.Annotations = map[string]string{
		api.AnnotationModels: "anthropic/claude-3-5-sonnet", // denied by cluster policy
	}

	err := k8sClient.Create(ctx, pod)
	if err == nil {
		t.Fatal("expected pod creation to be rejected; it was admitted")
	}
	if !errors.IsForbidden(err) {
		t.Fatalf("expected Forbidden, got: %v", err)
	}
}

// TestAdmission_NoDeclaration verifies that a pod with no model declaration is admitted
// without modification — token-control should be transparent for ungoverned workloads.
func TestAdmission_NoDeclaration(t *testing.T) {
	podName := "tc-test-nodecl"
	cleanupPod(t, testNamespace, podName)
	t.Cleanup(func() { cleanupPod(t, testNamespace, podName) })

	pod := testPod(podName, nil, "")
	// No annotation, no matching ModelClaim selector.

	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("expected ungoverned pod to be admitted, got: %v", err)
	}

	admittedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	// No credential env vars should be present.
	for _, c := range admittedPod.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "OPENAI_API_KEY" {
				t.Errorf("unexpected OPENAI_API_KEY injected into ungoverned pod")
			}
		}
	}
}

// TestModelClaim_BoundPhase verifies that the ModelClaim created by the test fixture
// reaches phase=Bound once the controller reconciles it against the cluster policy.
func TestModelClaim_BoundPhase(t *testing.T) {
	claim := &api.ModelClaim{}
	claimKey := client.ObjectKey{Namespace: testNamespace, Name: "test-claim"}

	if err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx, claimKey, claim); err != nil {
				return false, nil //nolint:nilerr
			}
			return claim.Status.Phase == api.ClaimBound, nil
		},
	); err != nil {
		t.Fatalf("ModelClaim did not reach Bound phase within %s; phase=%s conditions=%v",
			pollTimeout, claim.Status.Phase, claim.Status.Conditions)
	}
}

// assertEnvVarInjected checks that the first container of pod has an env var with the given name.
func assertEnvVarInjected(t *testing.T, pod *corev1.Pod, envName string) {
	t.Helper()
	if len(pod.Spec.Containers) == 0 {
		t.Errorf("pod has no containers")
		return
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == envName {
			return
		}
	}
	t.Errorf("env var %s not found in pod containers; found: %v",
		envName, envNames(pod.Spec.Containers[0].Env))
}

// assertAnnotationContains checks that the named annotation contains the given substring.
func assertAnnotationContains(t *testing.T, pod *corev1.Pod, key, want string) {
	t.Helper()
	val := pod.Annotations[key]
	if !strings.Contains(val, want) {
		t.Errorf("annotation %q = %q; expected to contain %q", key, val, want)
	}
}

func envNames(envs []corev1.EnvVar) []string {
	names := make([]string, len(envs))
	for i, e := range envs {
		names[i] = e.Name
	}
	return names
}

// testPod builds a minimal pause pod for admission testing. Pass app label and model
// annotation as needed; both may be empty.
func testPod(name string, extraLabels map[string]string, _ string) *corev1.Pod {
	labels := map[string]string{"tc-integration": "true"}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.9",
				},
			},
		},
	}
}
