// Package webhookv1alpha1 implements token-control's admission webhooks: structural and
// hierarchy validation for the governance CRDs, plus the pod validating/mutating webhooks
// that gate model usage and bind credentials at admission time (no proxy in the hot path).
package webhookv1alpha1

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/policy"
)

// Config holds configuration shared by the pod webhooks.
type Config struct {
	// OperatorNamespace is the namespace the controller runs in; it is always exempt.
	OperatorNamespace string
	// ExemptNamespaces are namespaces excluded from pod governance (e.g. kube-system).
	ExemptNamespaces map[string]struct{}
}

func (c Config) exempt(ns string) bool {
	if ns == "" {
		return true
	}
	if ns == c.OperatorNamespace {
		return true
	}
	_, ok := c.ExemptNamespaces[ns]
	return ok
}

// declaredModel is a provider/model pair a pod is governed for, sourced either from a
// matching ModelClaim (preferred) or the legacy models annotation (fallback).
type declaredModel struct {
	Provider string
	Model    string
	// Credential is an optional injection preference carried from a ModelClaim's per-model
	// credentialRef. It is used only as a fallback when the resolved policy names no
	// credential for the model; the policy hierarchy remains authoritative.
	Credential string
}

// parseModels reads the comma-separated "provider/model" declarations from a pod.
func parseModels(pod *corev1.Pod) []declaredModel {
	raw := pod.GetAnnotations()[api.AnnotationModels]
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []declaredModel
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.SplitN(tok, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, declaredModel{Provider: strings.TrimSpace(parts[0]), Model: strings.TrimSpace(parts[1])})
	}
	return out
}

// declaredModelsForPod returns the set of provider/model declarations that govern a pod,
// merging every ModelClaim whose selector matches the pod's identity (service account + pod
// labels) with the legacy models annotation. Declarations are de-duplicated case-insensitively
// by provider/model; a ModelClaim's credential preference is retained over the annotation's
// (which carries none). ModelClaims are the strongly-typed, RBAC-controlled successor to the
// self-asserted annotation, which is honored only as a fallback.
func declaredModelsForPod(ctx context.Context, c client.Client, namespace string, pod *corev1.Pod) ([]declaredModel, error) {
	type key struct{ p, m string }
	seen := map[key]int{}
	var out []declaredModel
	add := func(provider, model, cred string) {
		provider = strings.TrimSpace(provider)
		model = strings.TrimSpace(model)
		if provider == "" || model == "" {
			return
		}
		k := key{strings.ToLower(provider), strings.ToLower(model)}
		if idx, ok := seen[k]; ok {
			if out[idx].Credential == "" && cred != "" {
				out[idx].Credential = cred
			}
			return
		}
		seen[k] = len(out)
		out = append(out, declaredModel{Provider: provider, Model: model, Credential: cred})
	}

	var claims api.ModelClaimList
	if err := c.List(ctx, &claims, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	sort.Slice(claims.Items, func(i, j int) bool { return claims.Items[i].Name < claims.Items[j].Name })
	sa := saOf(pod)
	for i := range claims.Items {
		cl := &claims.Items[i]
		match, err := policy.WorkloadSelectorMatches(cl.Spec.Selector, pod.Labels, sa)
		if err != nil || !match {
			continue
		}
		for _, m := range cl.Spec.Models {
			cred := ""
			if m.CredentialRef != nil {
				cred = m.CredentialRef.Name
			}
			add(m.Provider, m.Model, cred)
		}
	}

	// Legacy annotation declarations carry no credential preference.
	for _, dm := range parseModels(pod) {
		add(dm.Provider, dm.Model, "")
	}
	return out, nil
}

func saOf(pod *corev1.Pod) string {
	if pod.Spec.ServiceAccountName != "" {
		return pod.Spec.ServiceAccountName
	}
	return "default"
}

// namespaceFromCtx prefers the namespace on the admission request, falling back to the object.
func namespaceFromCtx(ctx context.Context, fallback string) string {
	if req, err := admission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		return req.Namespace
	}
	return fallback
}

// resolveEffective lists the policy hierarchy and resolves the effective decision for a scope.
func resolveEffective(ctx context.Context, c client.Client, namespace string, podLabels map[string]string, sa string) (*policy.Effective, error) {
	var ctpl api.ClusterTokenPolicyList
	if err := c.List(ctx, &ctpl); err != nil {
		return nil, err
	}
	var tpl api.TokenPolicyList
	if err := c.List(ctx, &tpl, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	nsLabels, err := namespaceLabels(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	return policy.Resolve(policy.ResolveInput{
		ClusterPolicies:   ctpl.Items,
		NamespacePolicies: tpl.Items,
		Namespace:         namespace,
		NamespaceLabels:   nsLabels,
		PodLabels:         podLabels,
		ServiceAccount:    sa,
	})
}

func namespaceLabels(ctx context.Context, c client.Client, namespace string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return ns.Labels, nil
}

// credentialAuthorizes reports whether a namespace is permitted to bind a ModelCredential.
func credentialAuthorizes(ctx context.Context, c client.Client, mc *api.ModelCredential, namespace string) (bool, error) {
	for _, n := range mc.Spec.AllowedNamespaces {
		if n == namespace {
			return true, nil
		}
	}
	if mc.Spec.NamespaceSelector != nil {
		lbls, err := namespaceLabels(ctx, c, namespace)
		if err != nil {
			return false, err
		}
		ok, err := policy.NamespaceSelectorMatches(mc.Spec.NamespaceSelector, lbls)
		if err != nil {
			return false, err
		}
		return ok, nil
	}
	return false, nil
}

// defaultEnvName maps a provider to its conventional API-key environment variable.
func defaultEnvName(provider string) string {
	switch strings.ToLower(provider) {
	case "openai", "azure-openai", "azureopenai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "google", "gemini", "vertex", "vertexai":
		return "GOOGLE_API_KEY"
	case "bedrock", "aws":
		return "AWS_BEARER_TOKEN_BEDROCK"
	case "cohere":
		return "COHERE_API_KEY"
	case "mistral":
		return "MISTRAL_API_KEY"
	case "groq":
		return "GROQ_API_KEY"
	default:
		return strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"
	}
}

func setAnnotation(pod *corev1.Pod, key, value string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[key] = value
}

// invalid builds a Kubernetes Invalid status error for a typed object.
func invalid(kind, name string, msgs []string) error {
	return apierrors.NewBadRequest(fmt.Sprintf("%s %q is invalid: %s", kind, name, strings.Join(msgs, "; ")))
}

var _ = admission.Warnings{}
