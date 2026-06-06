package webhookv1alpha1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/policy"
)

// +kubebuilder:webhook:path=/validate-governance-tokencontrol-io-v1alpha1-modelcredential,mutating=false,failurePolicy=fail,sideEffects=None,groups=governance.tokencontrol.io,resources=modelcredentials,verbs=create;update,versions=v1alpha1,name=vmodelcredential.governance.tokencontrol.io,admissionReviewVersions=v1

// ModelCredentialValidator validates ModelCredential objects.
type ModelCredentialValidator struct {
	Client client.Client
	// OperatorNamespace is the default namespace for a SecretRef without an explicit namespace.
	OperatorNamespace string
}

var _ admission.CustomValidator = &ModelCredentialValidator{}

func (v *ModelCredentialValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *ModelCredentialValidator) ValidateUpdate(ctx context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *ModelCredentialValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ModelCredentialValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	mc, ok := obj.(*api.ModelCredential)
	if !ok {
		return nil, fmt.Errorf("expected a ModelCredential but got a %T", obj)
	}
	var hard []string
	var warnings admission.Warnings

	if mc.Spec.NamespaceSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(mc.Spec.NamespaceSelector); err != nil {
			hard = append(hard, fmt.Sprintf("spec.namespaceSelector is invalid: %v", err))
		}
	}
	for i, m := range mc.Spec.Models {
		if err := policy.ValidGlob(m); err != nil {
			hard = append(hard, fmt.Sprintf("spec.models[%d] %q is not a valid pattern: %v", i, m, err))
		}
	}
	if len(hard) > 0 {
		return warnings, invalid("ModelCredential", mc.Name, hard)
	}

	// Soft: a credential no namespace can bind is dead.
	if len(mc.Spec.AllowedNamespaces) == 0 && mc.Spec.NamespaceSelector == nil {
		warnings = append(warnings, "no namespace can bind this credential: set spec.allowedNamespaces or spec.namespaceSelector")
	}

	// Soft: source Secret/key existence.
	secretNS := mc.Spec.SecretRef.Namespace
	if secretNS == "" {
		secretNS = v.OperatorNamespace
	}
	var secret corev1.Secret
	if err := v.Client.Get(ctx, client.ObjectKey{Namespace: secretNS, Name: mc.Spec.SecretRef.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			warnings = append(warnings, fmt.Sprintf("source Secret %s/%s not found; credential will not be synced until it exists", secretNS, mc.Spec.SecretRef.Name))
		}
	} else if _, ok := secret.Data[mc.Spec.SecretRef.Key]; !ok {
		warnings = append(warnings, fmt.Sprintf("source Secret %s/%s has no key %q", secretNS, mc.Spec.SecretRef.Name, mc.Spec.SecretRef.Key))
	}
	return warnings, nil
}
