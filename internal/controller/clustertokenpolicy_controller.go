package controller

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/policy"
)

// ClusterTokenPolicyReconciler maintains the observed status of a ClusterTokenPolicy,
// in particular which namespaces it currently governs.
type ClusterTokenPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=clustertokenpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=clustertokenpolicies/status,verbs=get;update;patch

func (r *ClusterTokenPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ctp api.ClusterTokenPolicy
	if err := r.Get(ctx, req.NamespacedName, &ctp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var nsList corev1.NamespaceList
	if err := r.List(ctx, &nsList); err != nil {
		return ctrl.Result{}, err
	}
	var matched []string
	for i := range nsList.Items {
		ns := &nsList.Items[i]
		ok, err := policy.NamespaceSelectorMatches(ctp.Spec.NamespaceSelector, ns.Labels)
		if err != nil {
			setCondition(&ctp.Status.Conditions, api.ConditionValid, metav1.ConditionFalse, "BadSelector", err.Error(), ctp.Generation)
			return ctrl.Result{}, r.Status().Update(ctx, &ctp)
		}
		if ok {
			matched = append(matched, ns.Name)
		}
	}
	sort.Strings(matched)

	ctp.Status.ObservedGeneration = ctp.Generation
	ctp.Status.ModelCount = len(ctp.Spec.Models)
	ctp.Status.AppliedNamespaces = matched
	setCondition(&ctp.Status.Conditions, api.ConditionValid, metav1.ConditionTrue, "Validated", "spec is valid", ctp.Generation)
	setCondition(&ctp.Status.Conditions, api.ConditionReady, metav1.ConditionTrue, "Reconciled", "applied to selected namespaces", ctp.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, &ctp)
}

func (r *ClusterTokenPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.ClusterTokenPolicy{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.clusterPoliciesForAll)).
		Named("clustertokenpolicy").
		Complete(r)
}

func (r *ClusterTokenPolicyReconciler) clusterPoliciesForAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var ctpl api.ClusterTokenPolicyList
	if err := r.List(ctx, &ctpl); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(ctpl.Items))
	for i := range ctpl.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ctpl.Items[i])})
	}
	return reqs
}
