package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/metrics"
	"github.com/token-control/token-control/internal/policy"
)

// ModelClaimReconciler resolves a ModelClaim's requested models against the policy hierarchy
// and records the binding verdict in status. A ModelClaim is the bottom-up, workload-authored
// counterpart to the top-down grants in ClusterTokenPolicy/TokenPolicy: the claim says "these
// workloads intend to call these models", and binding succeeds only insofar as the hierarchy
// permits them. The controller never gates requests; it computes a durable preview of the
// admission decision (the authoritative per-pod verdict is still made by the pod webhook).
type ModelClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=modelclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=modelclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=clustertokenpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=tokenpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *ModelClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mcl api.ModelClaim
	if err := r.Get(ctx, req.NamespacedName, &mcl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var ctpl api.ClusterTokenPolicyList
	if err := r.List(ctx, &ctpl); err != nil {
		return ctrl.Result{}, err
	}
	var tpl api.TokenPolicyList
	if err := r.List(ctx, &tpl, client.InNamespace(mcl.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	nsLabels, err := r.namespaceLabels(ctx, mcl.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	resolved, err := r.resolveClaim(&mcl, ctpl.Items, tpl.Items, nsLabels)
	if err != nil {
		setCondition(&mcl.Status.Conditions, api.ConditionValid, metav1.ConditionFalse, "ResolveError", err.Error(), mcl.Generation)
		setCondition(&mcl.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "ResolveError", err.Error(), mcl.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, &mcl)
	}

	denied := 0
	for _, m := range resolved {
		if m.Action == api.ActionDeny {
			denied++
		}
	}
	mcl.Status.ResolvedModels = resolved
	mcl.Status.BoundCredentials = distinctAllowedCredentials(resolved)
	mcl.Status.ObservedGeneration = mcl.Generation

	if denied > 0 {
		mcl.Status.Phase = api.ClaimDenied
		setCondition(&mcl.Status.Conditions, api.ConditionBound, metav1.ConditionFalse, "ModelsDenied",
			"one or more requested models are not permitted by the effective policy", mcl.Generation)
	} else {
		mcl.Status.Phase = api.ClaimBound
		setCondition(&mcl.Status.Conditions, api.ConditionBound, metav1.ConditionTrue, "AllModelsPermitted",
			"every requested model is permitted by the effective policy", mcl.Generation)
	}
	setCondition(&mcl.Status.Conditions, api.ConditionValid, metav1.ConditionTrue, "Resolved", "claim resolved against the hierarchy", mcl.Generation)
	setCondition(&mcl.Status.Conditions, api.ConditionReady, metav1.ConditionTrue, "Reconciled", "claim binding computed", mcl.Generation)
	metrics.ModelClaimAllowedModels.WithLabelValues(mcl.Namespace, mcl.Name).Set(countAllowed(resolved))

	return ctrl.Result{}, r.Status().Update(ctx, &mcl)
}

// claimIdentity is a representative workload identity synthesized from a claim's selector,
// used to pull the workload tier of the hierarchy into the binding preview.
type claimIdentity struct {
	podLabels map[string]string
	sa        string
}

// claimIdentities derives the representative identities a claim's selector covers. A claim may
// select many workloads; the binding is conservative ("deny wins" across identities) so a model
// is Bound only if it is permitted for every representative identity. PodSelector.matchExpressions
// cannot be turned into a concrete label set and are therefore not synthesized here -- the
// authoritative, per-pod decision is always made later at admission.
func claimIdentities(sel *api.WorkloadSelector) []claimIdentity {
	if sel == nil {
		return []claimIdentity{{}}
	}
	var podLabels map[string]string
	if sel.PodSelector != nil && len(sel.PodSelector.MatchLabels) > 0 {
		podLabels = sel.PodSelector.MatchLabels
	}
	if len(sel.ServiceAccounts) == 0 {
		return []claimIdentity{{podLabels: podLabels}}
	}
	out := make([]claimIdentity, 0, len(sel.ServiceAccounts))
	for _, sa := range sel.ServiceAccounts {
		out = append(out, claimIdentity{podLabels: podLabels, sa: sa})
	}
	return out
}

// resolveClaim computes the per-request verdict for a claim across its representative
// identities. For each requested model the verdict is Allow only when every identity permits
// it; the bound credential is the most specific one the hierarchy names, falling back to the
// claim's own credentialRef preference when the hierarchy permits the model but names none.
func (r *ModelClaimReconciler) resolveClaim(mcl *api.ModelClaim, cluster []api.ClusterTokenPolicy, ns []api.TokenPolicy, nsLabels map[string]string) ([]api.EffectiveModel, error) {
	ids := claimIdentities(mcl.Spec.Selector)
	effs := make([]*policy.Effective, 0, len(ids))
	for _, id := range ids {
		eff, err := policy.Resolve(policy.ResolveInput{
			ClusterPolicies:   cluster,
			NamespacePolicies: ns,
			Namespace:         mcl.Namespace,
			NamespaceLabels:   nsLabels,
			PodLabels:         id.podLabels,
			ServiceAccount:    id.sa,
		})
		if err != nil {
			return nil, err
		}
		effs = append(effs, eff)
	}

	out := make([]api.EffectiveModel, 0, len(mcl.Spec.Models))
	for _, m := range mcl.Spec.Models {
		allowed := true
		credential := ""
		source := ""
		for _, eff := range effs {
			dec := eff.Permit(m.Provider, m.Model)
			if !dec.Allowed {
				allowed = false
				if source == "" {
					source = dec.DeniedBy
				}
				continue
			}
			if credential == "" && dec.Credential != "" {
				credential = dec.Credential
			}
		}
		action := api.ActionAllow
		if !allowed {
			action = api.ActionDeny
			credential = ""
		} else if credential == "" && m.CredentialRef != nil {
			// Permitted, but no policy named a credential: honor the claim's preference.
			credential = m.CredentialRef.Name
		}
		out = append(out, api.EffectiveModel{
			Provider:   m.Provider,
			Model:      m.Model,
			Action:     action,
			Credential: credential,
			Source:     source,
		})
	}
	return out, nil
}

func (r *ModelClaimReconciler) namespaceLabels(ctx context.Context, name string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		return map[string]string{}, client.IgnoreNotFound(err)
	}
	return ns.Labels, nil
}

func (r *ModelClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.ModelClaim{}).
		Watches(&api.ClusterTokenPolicy{}, handler.EnqueueRequestsFromMapFunc(r.claimsForAll)).
		Watches(&api.TokenPolicy{}, handler.EnqueueRequestsFromMapFunc(r.claimsForAll)).
		Watches(&api.ModelCredential{}, handler.EnqueueRequestsFromMapFunc(r.claimsForAll)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.claimsForAll)).
		Named("modelclaim").
		Complete(r)
}

// claimsForAll enqueues every ModelClaim when a cluster-scoped or policy input changes, since
// any of them can alter a claim's binding verdict.
func (r *ModelClaimReconciler) claimsForAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var mcll api.ModelClaimList
	if err := r.List(ctx, &mcll); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(mcll.Items))
	for i := range mcll.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&mcll.Items[i])})
	}
	return reqs
}
