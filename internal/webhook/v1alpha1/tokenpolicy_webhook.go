package webhookv1alpha1

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/policy"
)

// +kubebuilder:webhook:path=/validate-governance-tokencontrol-io-v1alpha1-tokenpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=governance.tokencontrol.io,resources=tokenpolicies,verbs=create;update,versions=v1alpha1,name=vtokenpolicy.governance.tokencontrol.io,admissionReviewVersions=v1

// TokenPolicyValidator validates TokenPolicy objects structurally and against the hierarchy.
type TokenPolicyValidator struct {
	Client client.Client
}

var _ admission.CustomValidator = &TokenPolicyValidator{}

func (v *TokenPolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *TokenPolicyValidator) ValidateUpdate(ctx context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *TokenPolicyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *TokenPolicyValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	tp, ok := obj.(*api.TokenPolicy)
	if !ok {
		return nil, fmt.Errorf("expected a TokenPolicy but got a %T", obj)
	}
	var hard []string
	var warnings admission.Warnings

	// Structural: selector parseability and glob validity.
	if tp.Spec.Selector != nil && tp.Spec.Selector.PodSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(tp.Spec.Selector.PodSelector); err != nil {
			hard = append(hard, fmt.Sprintf("spec.selector.podSelector is invalid: %v", err))
		}
	}
	hard = append(hard, validateModelRules(tp.Spec.Models)...)
	if tp.Spec.Gateway != nil && tp.Spec.Gateway.Type != api.GatewayNone && tp.Spec.Gateway.Type != "" && tp.Spec.Gateway.TargetRef == nil {
		hard = append(hard, fmt.Sprintf("spec.gateway.targetRef is required when spec.gateway.type is %q", tp.Spec.Gateway.Type))
	}
	if len(hard) > 0 {
		return warnings, invalid("TokenPolicy", tp.Name, hard)
	}

	// Soft checks that need cluster state. These never hard-fail (the runtime resolver is the
	// real control) but they flag dead rules and missing credentials early.
	warnings = append(warnings, v.credentialWarnings(ctx, tp.Spec.Models, "spec.models")...)
	warnings = append(warnings, v.hierarchyWarnings(ctx, tp)...)
	return warnings, nil
}

// hierarchyWarnings flags Allow rules that are already denied by a broader scope (and thus
// have no effect), reflecting the "narrow, never widen" invariant.
func (v *TokenPolicyValidator) hierarchyWarnings(ctx context.Context, tp *api.TokenPolicy) admission.Warnings {
	var warnings admission.Warnings

	var ctpl api.ClusterTokenPolicyList
	if err := v.Client.List(ctx, &ctpl); err != nil {
		return warnings
	}
	var tpl api.TokenPolicyList
	if err := v.Client.List(ctx, &tpl, client.InNamespace(tp.Namespace)); err != nil {
		return warnings
	}
	nsLabels, _ := namespaceLabels(ctx, v.Client, tp.Namespace)

	// Build the broader context: cluster tier + namespace defaults, excluding this policy.
	rest := make([]api.TokenPolicy, 0, len(tpl.Items))
	for _, p := range tpl.Items {
		if p.Name == tp.Name {
			continue
		}
		rest = append(rest, p)
	}
	base, err := policy.Resolve(policy.ResolveInput{
		ClusterPolicies:   ctpl.Items,
		NamespacePolicies: rest,
		Namespace:         tp.Namespace,
		NamespaceLabels:   nsLabels,
	})
	if err != nil || !base.ModelGoverned {
		return warnings
	}
	for _, m := range tp.Spec.Models {
		if m.Action == api.ActionDeny {
			continue
		}
		if d := base.Permit(m.Provider, m.Model); !d.Allowed {
			warnings = append(warnings, fmt.Sprintf(
				"Allow rule %s/%s has no effect: %s. A TokenPolicy can only narrow, never widen, the inherited permission set.",
				m.Provider, m.Model, d.Reason))
		}
	}
	return warnings
}

func (v *TokenPolicyValidator) credentialWarnings(ctx context.Context, models []api.ModelPermission, fieldPath string) admission.Warnings {
	var warnings admission.Warnings
	for _, m := range models {
		if m.CredentialRef == nil {
			continue
		}
		var mc api.ModelCredential
		if err := v.Client.Get(ctx, client.ObjectKey{Name: m.CredentialRef.Name}, &mc); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s references ModelCredential %q which was not found", fieldPath, m.CredentialRef.Name))
			continue
		}
		if mc.Spec.Provider != "" && m.Provider != "*" && !equalFold(mc.Spec.Provider, m.Provider) {
			warnings = append(warnings, fmt.Sprintf("%s binds credential %q (provider %q) to provider %q", fieldPath, m.CredentialRef.Name, mc.Spec.Provider, m.Provider))
		}
	}
	return warnings
}

// validateModelRules returns hard errors for malformed model globs.
func validateModelRules(models []api.ModelPermission) []string {
	var errs []string
	for i, m := range models {
		if err := policy.ValidGlob(m.Provider); err != nil {
			errs = append(errs, fmt.Sprintf("spec.models[%d].provider %q is not a valid pattern: %v", i, m.Provider, err))
		}
		if err := policy.ValidGlob(m.Model); err != nil {
			errs = append(errs, fmt.Sprintf("spec.models[%d].model %q is not a valid pattern: %v", i, m.Model, err))
		}
	}
	return errs
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
