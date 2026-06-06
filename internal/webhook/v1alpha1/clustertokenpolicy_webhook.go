package webhookv1alpha1

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/token-control/token-control/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-governance-tokencontrol-io-v1alpha1-clustertokenpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=governance.tokencontrol.io,resources=clustertokenpolicies,verbs=create;update,versions=v1alpha1,name=vclustertokenpolicy.governance.tokencontrol.io,admissionReviewVersions=v1

// ClusterTokenPolicyValidator validates ClusterTokenPolicy objects.
type ClusterTokenPolicyValidator struct {
	Client client.Client
}

var _ admission.CustomValidator = &ClusterTokenPolicyValidator{}

func (v *ClusterTokenPolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *ClusterTokenPolicyValidator) ValidateUpdate(ctx context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *ClusterTokenPolicyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClusterTokenPolicyValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	ctp, ok := obj.(*api.ClusterTokenPolicy)
	if !ok {
		return nil, fmt.Errorf("expected a ClusterTokenPolicy but got a %T", obj)
	}
	var hard []string
	var warnings admission.Warnings

	if ctp.Spec.NamespaceSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(ctp.Spec.NamespaceSelector); err != nil {
			hard = append(hard, fmt.Sprintf("spec.namespaceSelector is invalid: %v", err))
		}
	}
	hard = append(hard, validateModelRules(ctp.Spec.Models)...)
	if ctp.Spec.Gateway != nil && ctp.Spec.Gateway.Type != api.GatewayNone && ctp.Spec.Gateway.Type != "" && ctp.Spec.Gateway.TargetRef == nil {
		hard = append(hard, fmt.Sprintf("spec.gateway.targetRef is required when spec.gateway.type is %q", ctp.Spec.Gateway.Type))
	}
	if len(hard) > 0 {
		return warnings, invalid("ClusterTokenPolicy", ctp.Name, hard)
	}

	// Soft: warn on missing referenced credentials.
	tpv := &TokenPolicyValidator{Client: v.Client}
	warnings = append(warnings, tpv.credentialWarnings(ctx, ctp.Spec.Models, "spec.models")...)
	if ctp.Spec.DefaultCredentialRef != nil {
		var mc api.ModelCredential
		if err := v.Client.Get(ctx, client.ObjectKey{Name: ctp.Spec.DefaultCredentialRef.Name}, &mc); err != nil {
			warnings = append(warnings, fmt.Sprintf("spec.defaultCredentialRef references ModelCredential %q which was not found", ctp.Spec.DefaultCredentialRef.Name))
		}
	}
	return warnings, nil
}
