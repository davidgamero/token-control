//go:build integration

// Package integration contains end-to-end tests that run against a real Kubernetes cluster.
// The tests are skipped unless the integration build tag is provided:
//
//	go test -tags integration ./test/integration/...
//
// The test suite assumes a cluster is already reachable via the KUBECONFIG environment
// variable (or the default ~/.kube/config). Use the Makefile in this directory to provision
// a kind cluster, load images, and install token-control before running:
//
//	make -C test/integration setup
//	go test -tags integration -v -timeout 5m ./test/integration/...
//	make -C test/integration teardown
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/token-control/token-control/api/v1alpha1"
)

const (
	testNamespace = "tc-test"
	operatorNS    = "token-control-system"
	testAPIKey    = "sk-fake-integration-test-key"
	testdataDir   = "testdata"
	pollInterval  = 2 * time.Second
	pollTimeout   = 90 * time.Second
)

var (
	k8sClient  client.Client
	kubeClient kubernetes.Interface
	ctx        = context.Background()
)

func TestMain(m *testing.M) {
	if err := setupSuite(); err != nil {
		fmt.Fprintf(os.Stderr, "integration suite setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setupSuite() error {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}

	rc, err := client.New(cfg, client.Options{Scheme: buildScheme()})
	if err != nil {
		return fmt.Errorf("controller-runtime client: %w", err)
	}
	k8sClient = rc

	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}
	kubeClient = kc

	// Ensure the test namespace exists.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	_ = k8sClient.Create(ctx, ns)

	// Create the source API key Secret in the operator namespace.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openai-test",
			Namespace: operatorNS,
		},
		StringData: map[string]string{
			"apiKey": testAPIKey,
		},
	}
	_ = k8sClient.Create(ctx, secret)

	// Apply test fixtures in order.
	for _, f := range []string{
		filepath.Join(testdataDir, "01-policy.yaml"),
		filepath.Join(testdataDir, "02-modelclaim.yaml"),
		filepath.Join(testdataDir, "03-fake-llm.yaml"),
	} {
		if err := kubectlApply(f); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}

	// Wait for fake-llm to become ready before tests start.
	return waitForDeployment(testNamespace, "fake-llm")
}

// buildScheme returns a runtime.Scheme with core and governance types registered.
func buildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = api.AddToScheme(s)
	return s
}

// kubectlApply runs kubectl apply -f <path> against the current KUBECONFIG.
func kubectlApply(path string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitForDeployment polls until the named deployment has at least one ready replica.
func waitForDeployment(ns, name string) error {
	return wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			d, err := kubeClient.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil //nolint:nilerr
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	)
}

// waitForPodPhase polls until the named pod reaches the expected phase.
func waitForPodPhase(ns, name string, phase corev1.PodPhase) error {
	return wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pod, err := kubeClient.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil //nolint:nilerr
			}
			return pod.Status.Phase == phase, nil
		},
	)
}

// cleanupPod deletes the named pod, ignoring not-found errors.
func cleanupPod(t *testing.T, ns, name string) {
	t.Helper()
	_ = kubeClient.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
}
