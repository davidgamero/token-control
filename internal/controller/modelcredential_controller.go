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

	refs, err := r.referencingPolicies(ctx, mc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	mc.Status.ObservedGeneration = mc.Generation
	mc.Status.SyncedNamespaces = synced
	mc.Status.ReferencingPolicies = refs
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

func (r *ModelCredentialReconciler) referencingPolicies(ctx context.Context, name string) ([]string, error) {
	set := map[string]struct{}{}

	var ctpl api.ClusterTokenPolicyList
	if err := r.List(ctx, &ctpl); err != nil {
		return nil, err
	}
	for i := range ctpl.Items {
		ctp := &ctpl.Items[i]
		if ctp.Spec.DefaultCredentialRef != nil && ctp.Spec.DefaultCredentialRef.Name == name {
			set["ClusterTokenPolicy/"+ctp.Name] = struct{}{}
		}
		if modelsReference(ctp.Spec.Models, name) {
			set["ClusterTokenPolicy/"+ctp.Name] = struct{}{}
		}
	}

	var tpl api.TokenPolicyList
	if err := r.List(ctx, &tpl); err != nil {
		return nil, err
	}
	for i := range tpl.Items {
		tp := &tpl.Items[i]
		if modelsReference(tp.Spec.Models, name) {
			set[fmt.Sprintf("TokenPolicy/%s/%s", tp.Namespace, tp.Name)] = struct{}{}
		}
	}

	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
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
