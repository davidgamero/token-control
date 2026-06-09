//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	api "github.com/token-control/token-control/api/v1alpha1"
)

// fakeLLMBaseURL returns the URL of the fake LLM service reachable from within the cluster.
// Tests drive requests through a kubectl port-forward opened per test.
const fakeLLMPort = 18080 // local port for port-forward

// TestRouting_CredentialInjectedAndTrafficReaches verifies the end-to-end flow:
//  1. A pod with the right labels (matching the ModelClaim selector) is admitted.
//  2. The OPENAI_API_KEY env var is injected by the mutating webhook.
//  3. A Job running in the same namespace uses that credential to call the fake LLM service
//     and receives a valid chat completion response.
//  4. The fake LLM server's /requests log confirms the Authorization header arrived with the
//     expected key value.
func TestRouting_CredentialInjectedAndTrafficReaches(t *testing.T) {
	// Start a port-forward to the fake-llm service so tests can reach it from outside the cluster.
	pf, localURL, err := portForward(testNamespace, "svc/fake-llm", fakeLLMPort, 8080)
	if err != nil {
		t.Fatalf("port-forward fake-llm: %v", err)
	}
	t.Cleanup(pf.stop)

	// Reset the fake-llm request log.
	if err := fakeLLMPost(localURL+"/reset", nil); err != nil {
		t.Fatalf("reset fake-llm log: %v", err)
	}

	// Create a Job whose pod carries the ModelClaim selector labels so it gets the
	// credential injected, then calls the fake LLM via the injected env var.
	jobName := "tc-routing-job"
	cleanupJob(t, testNamespace, jobName)
	t.Cleanup(func() { cleanupJob(t, testNamespace, jobName) })

	job := callerJob(jobName, localURL)
	if _, err := kubeClient.BatchV1().Jobs(testNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create caller job: %v", err)
	}

	// Wait for the job to complete.
	if err := waitForJobCompletion(testNamespace, jobName); err != nil {
		printJobLogs(t, testNamespace, jobName)
		t.Fatalf("caller job did not complete: %v", err)
	}

	// Inspect the fake-llm request log.
	entries, err := fakeLLMGetRequests(localURL)
	if err != nil {
		t.Fatalf("read fake-llm request log: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("fake-llm received no requests; expected at least one chat completion call")
	}

	// Verify the correct model was called.
	var found bool
	for _, e := range entries {
		if e.Path == "/v1/chat/completions" && e.Model == "gpt-4o" {
			found = true
			// The Authorization header should contain the injected test API key prefix.
			if !strings.Contains(e.Authorization, "Bearer sk-fake") {
				t.Errorf("expected Authorization to contain injected key; got %q", e.Authorization)
			}
			break
		}
	}
	if !found {
		t.Errorf("no chat completion request for gpt-4o found in fake-llm log; entries: %+v", entries)
	}
}

// callerJob creates a Kubernetes Job whose pod:
//   - carries the app=tc-test-caller label (matches the ModelClaim selector)
//   - uses wget/curl to POST a chat completion to the fake LLM, relying on the injected
//     OPENAI_API_KEY environment variable for authentication.
func callerJob(name, fakeLLMURL string) *batchv1.Job {
	completions := int32(1)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: batchv1.JobSpec{
			Completions: &completions,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":            "tc-test-caller",
						"tc-integration": "true",
					},
					Annotations: map[string]string{
						api.AnnotationModels: "openai/gpt-4o",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "caller",
							Image: "curlimages/curl:8.7.1",
							Command: []string{
								"sh", "-c",
								fmt.Sprintf(`curl -sf -X POST %s/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}' \
  | grep -q "fake response from gpt-4o" && echo "OK" || exit 1`,
									fakeLLMURL),
							},
						},
					},
				},
			},
		},
	}
}

// portForward starts `kubectl port-forward` and returns a closer + the local base URL.
func portForward(ns, resource string, localPort, remotePort int) (*pfProcess, string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", ns, resource,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	url := fmt.Sprintf("http://localhost:%d", localPort)

	// Wait until the port-forward is accepting connections.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return &pfProcess{cmd}, url, nil
}

type pfProcess struct{ cmd *exec.Cmd }

func (p *pfProcess) stop() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// waitForJobCompletion polls until the job has at least one succeeded pod.
func waitForJobCompletion(ns, name string) error {
	return wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			j, err := kubeClient.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil //nolint:nilerr
			}
			if j.Status.Failed > 0 {
				return false, fmt.Errorf("job %s/%s failed", ns, name)
			}
			return j.Status.Succeeded > 0, nil
		},
	)
}

// cleanupJob deletes the named job and its pods.
func cleanupJob(t *testing.T, ns, name string) {
	t.Helper()
	prop := metav1.DeletePropagationForeground
	_ = kubeClient.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
}

// printJobLogs prints logs from all pods of the job for debugging.
func printJobLogs(t *testing.T, ns, jobName string) {
	t.Helper()
	pods, err := kubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return
	}
	for _, pod := range pods.Items {
		logs, err := kubeClient.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if err != nil {
			continue
		}
		t.Logf("pod %s logs:\n%s", pod.Name, string(logs))
	}
}

type logEntry struct {
	Time          time.Time `json:"time"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Authorization string    `json:"authorization"`
	Model         string    `json:"model,omitempty"`
}

func fakeLLMPost(url string, body any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	resp, err := http.Post(url, "application/json", r) //nolint:noctx
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func fakeLLMGetRequests(baseURL string) ([]logEntry, error) {
	resp, err := http.Get(baseURL + "/requests") //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var entries []logEntry
	return entries, json.NewDecoder(resp.Body).Decode(&entries)
}
