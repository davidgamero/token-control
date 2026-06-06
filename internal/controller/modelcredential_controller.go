package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/metrics"
	"github.com/token-control/token-control/internal/policy"
)

// ModelCredentialReconciler resolves a ModelCredential's source Secret and replicates it
// (centrally managed, owner-referenced, labeled) into the namespaces authorized to bind it.
// This is what removes "Secret sprawl": teams declare intent, the controller owns key
// material distribution and cleanup.
type ModelCredentialReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// SecretClient is an uncached client used for all Secret IO so the manager does not
	// cache every Secret in the cluster.
	SecretClient client.Client
	// OperatorNamespace is the default namespace for a SecretRef without an explicit one.
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=modelcredentials,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.tokencontrol.io,resources=modelcredentials/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *ModelCredentialReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var mc api.ModelCredential
	if err := r.Get(ctx, req.NamespacedName, &mc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	secretNS := mc.Spec.SecretRef.Namespace
	if secretNS == "" {
		secretNS = r.OperatorNamespace
	}

	var src corev1.Secret
	if err := r.SecretClient.Get(ctx, client.ObjectKey{Namespace: secretNS, Name: mc.Spec.SecretRef.Name}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			mc.Status.SecretResolved = false
			setCondition(&mc.Status.Conditions, api.ConditionSecretResolved, metav1.ConditionFalse, "SecretNotFound",
				fmt.Sprintf("source Secret %s/%s not found", secretNS, mc.Spec.SecretRef.Name), mc.Generation)
			setCondition(&mc.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "SecretNotFound", "waiting for source Secret", mc.Generation)
			return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, &mc)
		}
		return ctrl.Result{}, err
	}
	data, ok := src.Data[mc.Spec.SecretRef.Key]
	if !ok {
		mc.Status.SecretResolved = false
		setCondition(&mc.Status.Conditions, api.ConditionSecretResolved, metav1.ConditionFalse, "KeyMissing",
			fmt.Sprintf("source Secret %s/%s has no key %q", secretNS, mc.Spec.SecretRef.Name, mc.Spec.SecretRef.Key), mc.Generation)
		return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, &mc)
	}
	mc.Status.SecretResolved = true
	setCondition(&mc.Status.Conditions, api.ConditionSecretResolved, metav1.ConditionTrue, "Resolved", "source Secret resolved", mc.Generation)

	authorized, err := r.authorizedNamespaces(ctx, &mc)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Replicate into each authorized namespace.
	synced := make([]string, 0, len(authorized))
	for ns := range authorized {
		if err := r.syncSecret(ctx, &mc, ns, data); err != nil {
			log.Error(err, "failed to sync credential secret", "namespace", ns)
			continue
		}
		synced = append(synced, ns)
	}
	sort.Strings(synced)

	// Garbage-collect replicas in namespaces that are no longer authorized.
	if err := r.pruneSecrets(ctx, &mc, authorized); err != nil {
		log.Error(err, "failed to prune stale credential secrets")
	}

	refs, allocated, err := r.referencingPolicies(ctx, mc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	mc.Status.ObservedGeneration = mc.Generation
	mc.Status.SyncedNamespaces = synced
	mc.Status.ReferencingPolicies = refs
	r.reconcileCapacity(&mc, allocated)
	setCondition(&mc.Status.Conditions, api.ConditionReady, metav1.ConditionTrue, "Synced",
		fmt.Sprintf("synced to %d namespace(s)", len(synced)), mc.Generation)
	metrics.CredentialSyncedNamespaces.WithLabelValues(mc.Name).Set(float64(len(synced)))

	// Periodic resync to heal drift on the (uncached) managed Secrets.
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, r.Status().Update(ctx, &mc)
}

func (r *ModelCredentialReconciler) authorizedNamespaces(ctx context.Context, mc *api.ModelCredential) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, n := range mc.Spec.AllowedNamespaces {
		out[n] = struct{}{}
	}
	if mc.Spec.NamespaceSelector != nil {
		var nsList corev1.NamespaceList
		if err := r.List(ctx, &nsList); err != nil {
			return nil, err
		}
		for i := range nsList.Items {
			ns := &nsList.Items[i]
			ok, err := policy.NamespaceSelectorMatches(mc.Spec.NamespaceSelector, ns.Labels)
			if err != nil {
				return nil, err
			}
			if ok {
				out[ns.Name] = struct{}{}
			}
		}
	}
	return out, nil
}

func (r *ModelCredentialReconciler) syncSecret(ctx context.Context, mc *api.ModelCredential, ns string, data []byte) error {
	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns,
		Name:      api.ManagedSecretPrefix + mc.Name,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.SecretClient, managed, func() error {
		if managed.Labels == nil {
			managed.Labels = map[string]string{}
		}
		managed.Labels[api.LabelManagedBy] = api.ManagedByValue
		managed.Labels[api.LabelCredential] = mc.Name
		if managed.Annotations == nil {
			managed.Annotations = map[string]string{}
		}
		managed.Annotations["governance.tokencontrol.io/source"] = fmt.Sprintf("%s/%s", firstNonEmpty(mc.Spec.SecretRef.Namespace, r.OperatorNamespace), mc.Spec.SecretRef.Name)
		managed.Type = corev1.SecretTypeOpaque
		managed.Data = map[string][]byte{mc.Spec.SecretRef.Key: data}
		// Cluster-scoped owner of a namespaced dependent: valid, and enables GC on delete.
		return controllerutil.SetControllerReference(mc, managed, r.Scheme)
	})
	return err
}

func (r *ModelCredentialReconciler) pruneSecrets(ctx context.Context, mc *api.ModelCredential, authorized map[string]struct{}) error {
	var secrets corev1.SecretList
	if err := r.SecretClient.List(ctx, &secrets, client.MatchingLabels{api.LabelCredential: mc.Name}); err != nil {
		return err
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if _, ok := authorized[s.Namespace]; ok {
			continue
		}
		if err := r.SecretClient.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// referencingPolicies returns the policies that bind this credential and the planning
// rollup of their token budgets. A policy "binds" the credential when one of its Allow
// rules names it via credentialRef, or (for a ClusterTokenPolicy) when it is the default
// credential. Each binding policy's spec.quota is summed field-wise into allocated; this is
// a coarse planning estimate of committed demand, not a precise per-namespace reservation.
func (r *ModelCredentialReconciler) referencingPolicies(ctx context.Context, name string) ([]string, *api.TokenQuota, error) {
	set := map[string]struct{}{}
	allocated := &api.TokenQuota{}

	var ctpl api.ClusterTokenPolicyList
	if err := r.List(ctx, &ctpl); err != nil {
		return nil, nil, err
	}
	for i := range ctpl.Items {
		ctp := &ctpl.Items[i]
		key := "ClusterTokenPolicy/" + ctp.Name
		isDefault := ctp.Spec.DefaultCredentialRef != nil && ctp.Spec.DefaultCredentialRef.Name == name
		if isDefault || modelsReference(ctp.Spec.Models, name) {
			if _, seen := set[key]; !seen {
				set[key] = struct{}{}
				allocated = addQuota(allocated, ctp.Spec.Quota)
			}
		}
	}

	var tpl api.TokenPolicyList
	if err := r.List(ctx, &tpl); err != nil {
		return nil, nil, err
	}
	for i := range tpl.Items {
		tp := &tpl.Items[i]
		if modelsReference(tp.Spec.Models, name) {
			key := fmt.Sprintf("TokenPolicy/%s/%s", tp.Namespace, tp.Name)
			if _, seen := set[key]; !seen {
				set[key] = struct{}{}
				allocated = addQuota(allocated, tp.Spec.Quota)
			}
		}
	}

	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, normalizeQuota(allocated), nil
}

func modelsReference(models []api.ModelPermission, name string) bool {
	for _, m := range models {
		if m.CredentialRef != nil && m.CredentialRef.Name == name {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reconcileCapacity records the supply/demand planning rollup for a credential: allocated
// (the summed budgets of referencing policies) versus the declared capacity, surfacing
// available headroom and an Oversubscribed condition. Capacity is advisory and never blocks
// reconciliation; live enforcement remains the gateway's responsibility.
func (r *ModelCredentialReconciler) reconcileCapacity(mc *api.ModelCredential, allocated *api.TokenQuota) {
	mc.Status.Allocated = allocated

	capacity := mc.Spec.Capacity
	if capacity == nil {
		mc.Status.Available = nil
		setCondition(&mc.Status.Conditions, api.ConditionOversubscribed, metav1.ConditionFalse,
			"NoCapacityDeclared", "spec.capacity is not set; capacity planning is disabled", mc.Generation)
		metrics.CredentialCapacityTPM.WithLabelValues(mc.Name).Set(0)
		metrics.CredentialAllocatedTPM.WithLabelValues(mc.Name).Set(quotaTPM(allocated))
		metrics.CredentialOversubscribed.WithLabelValues(mc.Name).Set(0)
		return
	}

	mc.Status.Available = availableQuota(capacity, allocated)
	over := oversubscribed(capacity, allocated)
	if over {
		setCondition(&mc.Status.Conditions, api.ConditionOversubscribed, metav1.ConditionTrue,
			"AllocationExceedsCapacity", "committed policy budgets exceed the declared key capacity", mc.Generation)
	} else {
		setCondition(&mc.Status.Conditions, api.ConditionOversubscribed, metav1.ConditionFalse,
			"WithinCapacity", "committed policy budgets are within the declared key capacity", mc.Generation)
	}
	metrics.CredentialCapacityTPM.WithLabelValues(mc.Name).Set(quotaTPM(capacity))
	metrics.CredentialAllocatedTPM.WithLabelValues(mc.Name).Set(quotaTPM(allocated))
	metrics.CredentialOversubscribed.WithLabelValues(mc.Name).Set(b2f(over))
}

// addQuota returns the field-wise sum of two quotas, treating nil fields as zero.
func addQuota(a, b *api.TokenQuota) *api.TokenQuota {
	return mapQuota2(a, b, func(x, y *int64) *int64 {
		switch {
		case x == nil && y == nil:
			return nil
		case x == nil:
			v := *y
			return &v
		case y == nil:
			v := *x
			return &v
		default:
			v := *x + *y
			return &v
		}
	})
}

// availableQuota returns capacity minus allocated per field, floored at zero. Fields where
// capacity is unset stay nil (no capacity declared for that window).
func availableQuota(capacity, allocated *api.TokenQuota) *api.TokenQuota {
	return mapQuota2(capacity, allocated, func(cv, av *int64) *int64 {
		if cv == nil {
			return nil
		}
		used := int64(0)
		if av != nil {
			used = *av
		}
		v := *cv - used
		if v < 0 {
			v = 0
		}
		return &v
	})
}

// oversubscribed reports whether any window's allocated demand exceeds the declared capacity.
func oversubscribed(capacity, allocated *api.TokenQuota) bool {
	if capacity == nil || allocated == nil {
		return false
	}
	over := func(cv, av *int64) bool { return cv != nil && av != nil && *av > *cv }
	return over(capacity.TokensPerMinute, allocated.TokensPerMinute) ||
		over(capacity.RequestsPerMinute, allocated.RequestsPerMinute) ||
		over(capacity.TokensPerDay, allocated.TokensPerDay) ||
		over(capacity.TokensPerMonth, allocated.TokensPerMonth)
}

// mapQuota2 applies op to each corresponding field of a and b and normalizes the result.
func mapQuota2(a, b *api.TokenQuota, op func(x, y *int64) *int64) *api.TokenQuota {
	if a == nil {
		a = &api.TokenQuota{}
	}
	if b == nil {
		b = &api.TokenQuota{}
	}
	return normalizeQuota(&api.TokenQuota{
		TokensPerMinute:   op(a.TokensPerMinute, b.TokensPerMinute),
		RequestsPerMinute: op(a.RequestsPerMinute, b.RequestsPerMinute),
		TokensPerDay:      op(a.TokensPerDay, b.TokensPerDay),
		TokensPerMonth:    op(a.TokensPerMonth, b.TokensPerMonth),
	})
}

// normalizeQuota returns nil when every field is unset so empty quotas don't appear in status.
func normalizeQuota(q *api.TokenQuota) *api.TokenQuota {
	if q == nil {
		return nil
	}
	if q.TokensPerMinute == nil && q.RequestsPerMinute == nil && q.TokensPerDay == nil && q.TokensPerMonth == nil {
		return nil
	}
	return q
}

func quotaTPM(q *api.TokenQuota) float64 {
	if q == nil || q.TokensPerMinute == nil {
		return 0
	}
	return float64(*q.TokensPerMinute)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (r *ModelCredentialReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.ModelCredential{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.credentialsForAll)).
		Named("modelcredential").
		Complete(r)
}

func (r *ModelCredentialReconciler) credentialsForAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var mcl api.ModelCredentialList
	if err := r.List(ctx, &mcl); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(mcl.Items))
	for i := range mcl.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&mcl.Items[i])})
	}
	return reqs
}
