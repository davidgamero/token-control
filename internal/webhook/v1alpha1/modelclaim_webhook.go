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

// +kubebuilder:webhook:path=/validate-governance-tokencontrol-io-v1alpha1-modelclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=governance.tokencontrol.io,resources=modelclaims,verbs=create;update,versions=v1alpha1,name=vmodelclaim.governance.tokencontrol.io,admissionReviewVersions=v1

// ModelClaimValidator validates ModelClaim objects structurally and, softly, against the
// policy hierarchy. Structural problems are hard errors; a requested model the hierarchy
// refuses is surfaced as a warning (and durably as status.phase=Denied by the controller),
// consistent with token-control's "the resolver is the real control" philosophy.
type ModelClaimValidator struct {
	Client client.Client
}

var _ admission.CustomValidator = &ModelClaimValidator{}

func (v *ModelClaimValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *ModelClaimValidator) ValidateUpdate(ctx context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *ModelClaimValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ModelClaimValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	mc, ok := obj.(*api.ModelClaim)
	if !ok {
		return nil, fmt.Errorf("expected a ModelClaim but got a %T", obj)
	}
	var hard []string
	var warnings admission.Warnings

	// Structural: selector parseability and model-name validity.
	if mc.Spec.Selector != nil && mc.Spec.Selector.PodSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(mc.Spec.Selector.PodSelector); err != nil {
			hard = append(hard, fmt.Sprintf("spec.selector.podSelector is invalid: %v", err))
		}
	}
	for i, m := range mc.Spec.Models {
		if err := policy.ValidGlob(m.Provider); err != nil {
			hard = append(hard, fmt.Sprintf("spec.models[%d].provider %q is not a valid pattern: %v", i, m.Provider, err))
		}
		if err := policy.ValidGlob(m.Model); err != nil {
			hard = append(hard, fmt.Sprintf("spec.models[%d].model %q is not a valid pattern: %v", i, m.Model, err))
		}
	}
	if len(hard) > 0 {
		return warnings, invalid("ModelClaim", mc.Name, hard)
	}

	// Soft: referenced credentials should exist, and the requested models should be permitted
	// by the broad (cluster + namespace-default) tiers. The controller does the full
	// per-workload resolution and records the durable verdict in status.
	for i, m := range mc.Spec.Models {
		if m.CredentialRef == nil {
			continue
		}
		var cred api.ModelCredential
		if err := v.Client.Get(ctx, client.ObjectKey{Name: m.CredentialRef.Name}, &cred); err != nil {
			warnings = append(warnings, fmt.Sprintf("spec.models[%d] references ModelCredential %q which was not found", i, m.CredentialRef.Name))
		}
	}
	warnings = append(warnings, v.permitWarnings(ctx, mc)...)
	return warnings, nil
}

// permitWarnings flags requested models that the cluster + namespace-default tiers already
// deny, so the author gets immediate feedback that the claim will not bind.
func (v *ModelClaimValidator) permitWarnings(ctx context.Context, mc *api.ModelClaim) admission.Warnings {
	var warnings admission.Warnings

	var ctpl api.ClusterTokenPolicyList
	if err := v.Client.List(ctx, &ctpl); err != nil {
		return warnings
	}
	var tpl api.TokenPolicyList
	if err := v.Client.List(ctx, &tpl, client.InNamespace(mc.Namespace)); err != nil {
		return warnings
	}
	nsLabels, _ := namespaceLabels(ctx, v.Client, mc.Namespace)
	eff, err := policy.Resolve(policy.ResolveInput{
		ClusterPolicies:   ctpl.Items,
		NamespacePolicies: tpl.Items,
		Namespace:         mc.Namespace,
		NamespaceLabels:   nsLabels,
	})
	if err != nil || !eff.ModelGoverned {
		return warnings
	}
	for _, m := range mc.Spec.Models {
		if d := eff.Permit(m.Provider, m.Model); !d.Allowed {
			warnings = append(warnings, fmt.Sprintf(
				"requested model %s/%s will not bind: %s", m.Provider, m.Model, d.Reason))
		}
	}
	return warnings
}
