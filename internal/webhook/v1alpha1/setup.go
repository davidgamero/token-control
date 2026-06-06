package webhookv1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	api "github.com/token-control/token-control/api/v1alpha1"
)

// SetupWebhooksWithManager registers all token-control admission webhooks with the manager.
func SetupWebhooksWithManager(mgr ctrl.Manager, cfg Config) error {
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&api.ClusterTokenPolicy{}).
		WithValidator(&ClusterTokenPolicyValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return err
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&api.TokenPolicy{}).
		WithValidator(&TokenPolicyValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return err
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&api.ModelCredential{}).
		WithValidator(&ModelCredentialValidator{Client: mgr.GetClient(), OperatorNamespace: cfg.OperatorNamespace}).
		Complete(); err != nil {
		return err
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&api.ModelClaim{}).
		WithValidator(&ModelClaimValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return err
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithValidator(&PodValidator{Client: mgr.GetClient(), Config: cfg}).
		WithDefaulter(&PodDefaulter{Client: mgr.GetClient(), Config: cfg}).
		Complete(); err != nil {
		return err
	}

	return nil
}
