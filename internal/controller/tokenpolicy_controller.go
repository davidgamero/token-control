package controller

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/gateway"
	"github.com/token-control/token-control/internal/metrics"
	"github.com/token-control/token-control/internal/policy"
)

// TokenPolicyReconciler resolves a TokenPolicy into status and manages its gateway artifact.
type TokenPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Apply is an uncached client used to server-side-apply unstructured gateway artifacts
	// without starting informers for gateway CRDs that may not be installed.
	Apply client.Client
}

// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=tokenpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=tokenpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=tokenpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=clustertokenpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=modelcredentials,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuadrant.io,resources=tokenratelimitpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *TokenPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var tp api.TokenPolicy
	if err := r.Get(ctx, req.NamespacedName, &tp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var ctpl api.ClusterTokenPolicyList
	if err := r.List(ctx, &ctpl); err != nil {
		return ctrl.Result{}, err
	}
	var tpl api.TokenPolicyList
	if err := r.List(ctx, &tpl, client.InNamespace(tp.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	nsLabels, err := r.namespaceLabels(ctx, tp.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	eff, err := policy.ResolveForPolicy(ctpl.Items, tpl.Items, nsLabels, tp)
	if err != nil {
		setCondition(&tp.Status.Conditions, api.ConditionValid, metav1.ConditionFalse, "ResolveError", err.Error(), tp.Generation)
		setCondition(&tp.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "ResolveError", err.Error(), tp.Generation)
		return ctrl.Result{}, r.updateStatus(ctx, &tp)
	}

	models := eff.EffectiveModels()
	tp.Status.EffectiveModels = models
	tp.Status.EffectiveQuota = eff.Quota
	tp.Status.BoundCredentials = distinctAllowedCredentials(models)
	tp.Status.ObservedGeneration = tp.Generation
	setCondition(&tp.Status.Conditions, api.ConditionValid, metav1.ConditionTrue, "Resolved", "policy resolved against the hierarchy", tp.Generation)
	setCondition(&tp.Status.Conditions, api.ConditionReady, metav1.ConditionTrue, "Reconciled", "effective policy computed", tp.Generation)
	metrics.EffectiveModels.WithLabelValues(tp.Namespace, tp.Name).Set(countAllowed(models))

	// Gateway artifact generation (optional, best-effort).
	if err := r.reconcileGateway(ctx, &tp, eff); err != nil {
		log.Error(err, "gateway artifact reconcile failed")
	}

	return ctrl.Result{}, r.updateStatus(ctx, &tp)
}

func (r *TokenPolicyReconciler) reconcileGateway(ctx context.Context, tp *api.TokenPolicy, eff *policy.Effective) error {
	gw := eff.Gateway
	desired, err := gateway.For(gw, tp, eff.Quota)
	if err != nil {
		setCondition(&tp.Status.Conditions, api.ConditionGatewaySynced, metav1.ConditionFalse, "InvalidConfig", err.Error(), tp.Generation)
		return err
	}

	// Clean up artifacts of types that are no longer selected.
	keep := schema.GroupVersionKind{}
	if desired != nil {
		keep = desired.GroupVersionKind()
	}
	for _, gvk := range []schema.GroupVersionKind{gateway.EnvoyBackendTrafficPolicyGVK, gateway.KuadrantTokenRateLimitPolicyGVK} {
		if gvk == keep {
			continue
		}
		r.deleteArtifact(ctx, gvk, tp.Namespace, gateway.ArtifactName(tp.Name))
	}

	if desired == nil {
		tp.Status.GatewayRef = ""
		metrics.GatewayArtifacts.WithLabelValues(tp.Namespace, "None").Set(0)
		setCondition(&tp.Status.Conditions, api.ConditionGatewaySynced, metav1.ConditionTrue, "Disabled", "no gateway integration configured", tp.Generation)
		return nil
	}

	if err := controllerutil.SetControllerReference(tp, desired, r.Scheme); err != nil {
		return err
	}
	if err := r.Apply.Patch(ctx, desired, client.Apply, client.FieldOwner("token-control"), client.ForceOwnership); err != nil {
		reason := "ApplyFailed"
		if apimeta.IsNoMatchError(err) {
			reason = "GatewayCRDNotInstalled"
		}
		setCondition(&tp.Status.Conditions, api.ConditionGatewaySynced, metav1.ConditionFalse, reason, err.Error(), tp.Generation)
		return err
	}
	tp.Status.GatewayRef = desired.GetKind() + "/" + desired.GetName()
	metrics.GatewayArtifacts.WithLabelValues(tp.Namespace, string(gw.Type)).Set(1)
	setCondition(&tp.Status.Conditions, api.ConditionGatewaySynced, metav1.ConditionTrue, "Synced", "gateway artifact applied", tp.Generation)
	return nil
}

func (r *TokenPolicyReconciler) deleteArtifact(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = r.Apply.Delete(ctx, u)
}

func (r *TokenPolicyReconciler) namespaceLabels(ctx context.Context, name string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		return map[string]string{}, client.IgnoreNotFound(err)
	}
	return ns.Labels, nil
}

func (r *TokenPolicyReconciler) updateStatus(ctx context.Context, tp *api.TokenPolicy) error {
	return r.Status().Update(ctx, tp)
}

func (r *TokenPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.TokenPolicy{}).
		Watches(&api.ClusterTokenPolicy{}, handler.EnqueueRequestsFromMapFunc(r.tokenPoliciesForAll)).
		Watches(&api.ModelCredential{}, handler.EnqueueRequestsFromMapFunc(r.tokenPoliciesForAll)).
		Named("tokenpolicy").
		Complete(r)
}

// tokenPoliciesForAll enqueues every TokenPolicy when a cluster-scoped input changes.
func (r *TokenPolicyReconciler) tokenPoliciesForAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var tpl api.TokenPolicyList
	if err := r.List(ctx, &tpl); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(tpl.Items))
	for i := range tpl.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tpl.Items[i])})
	}
	return reqs
}
