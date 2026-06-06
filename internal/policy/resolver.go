// Package policy implements the effective-policy resolver that is the heart of
// token-control's governance plane. It deterministically combines the three tiers
// of the policy hierarchy -- cluster (ClusterTokenPolicy), namespace (a TokenPolicy
// with an empty selector) and workload (a TokenPolicy whose selector matches a pod) --
// into a single decision for "is this provider/model permitted, with which credential,
// under what quota and enforcement mode".
//
// # Merge semantics
//
// Tiers are ordered broad -> narrow: cluster, namespace, workload. The cardinal rule
// is *narrow, never widen*: a more specific tier may only remove models or tighten a
// quota relative to the tiers above it.
//
// For a given provider/model M, each tier returns one of three decisions:
//
//   - Deny    : the tier explicitly denies M, OR the tier publishes an allowlist
//     (>=1 Allow rule) that M does not match (deny-by-omission).
//   - Allow   : the tier matches M with an Allow rule, OR the tier is a pure denylist
//     (only Deny rules) that does not match M.
//   - Abstain : the tier declares no model rules at all and therefore inherits.
//
// M is permitted iff *no* tier returns Deny. Because a narrower allowlist can only
// introduce deny-by-omission, a narrower tier can never re-permit something a broader
// tier denies. If every tier abstains, model allowlisting is simply not configured for
// the scope and admission is not gated on the model dimension.
package policy

import (
	"fmt"
	"path"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	api "github.com/token-control/token-control/api/v1alpha1"
)

// ResolveInput is the immutable set of cluster state used to compute an effective policy.
type ResolveInput struct {
	// ClusterPolicies is every ClusterTokenPolicy in the cluster.
	ClusterPolicies []api.ClusterTokenPolicy
	// NamespacePolicies is every TokenPolicy in the target namespace.
	NamespacePolicies []api.TokenPolicy
	// Namespace is the target namespace name.
	Namespace string
	// NamespaceLabels are the labels on the target namespace, used for selector matching.
	NamespaceLabels map[string]string

	// The following identify a concrete workload. They are optional: when both are zero
	// the resolver evaluates the namespace-default scope only (cluster + namespace tiers).
	PodLabels      map[string]string
	ServiceAccount string
}

// Decision is a single tier's verdict for a provider/model.
type Decision int

const (
	// Abstain means the tier declares no model rules and therefore inherits.
	Abstain Decision = iota
	// Allow means the tier permits the model.
	Allow
	// Deny means the tier forbids the model (explicitly or by omission).
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "Allow"
	case Deny:
		return "Deny"
	default:
		return "Abstain"
	}
}

// sourcedRule pairs a model rule with the policy that declared it.
type sourcedRule struct {
	rule   api.ModelPermission
	source string
}

// tier is a flattened set of model rules contributed by one level of the hierarchy.
type tier struct {
	name  string
	rules []sourcedRule
}

// Effective is the resolved governance decision for a scope.
type Effective struct {
	// Governed is true when at least one policy applies to the scope.
	Governed bool
	// ModelGoverned is true when model allowlisting is configured (some tier has model rules).
	ModelGoverned bool
	// Enforcement is the resolved enforcement mode (most specific non-empty; default Enforce).
	Enforcement api.EnforcementMode
	// Quota is the most restrictive quota across all applicable tiers.
	Quota *api.TokenQuota
	// Gateway is the most specific gateway integration across applicable tiers.
	Gateway *api.GatewayIntegration
	// DefaultCredential is the cluster default credential name (fallback for allowed models).
	DefaultCredential string
	// Sources lists the policies that contributed to this decision ("Kind/namespace/name").
	Sources []string

	cluster tier
	ns      tier
	wl      tier
}

// ModelDecision is the resolved verdict for a single provider/model query.
type ModelDecision struct {
	Provider   string
	Model      string
	Allowed    bool
	Credential string
	// DeniedBy names the tier and policy that produced a Deny, when Allowed is false.
	DeniedBy string
	// Reason is a human-readable explanation suitable for an admission response.
	Reason string
}

// Resolve combines the policy hierarchy for the ResolveInput into an Effective decision.
func Resolve(in ResolveInput) (*Effective, error) {
	eff := &Effective{Enforcement: api.EnforcementEnforce}
	sourceSet := map[string]struct{}{}

	// --- cluster tier ---
	var clusterMatched []api.ClusterTokenPolicy
	for i := range in.ClusterPolicies {
		ctp := in.ClusterPolicies[i]
		match, err := selectorMatches(ctp.Spec.NamespaceSelector, in.NamespaceLabels)
		if err != nil {
			return nil, fmt.Errorf("ClusterTokenPolicy %q namespaceSelector: %w", ctp.Name, err)
		}
		if !match {
			continue
		}
		clusterMatched = append(clusterMatched, ctp)
	}
	// Deterministic ordering: by name.
	sort.Slice(clusterMatched, func(i, j int) bool { return clusterMatched[i].Name < clusterMatched[j].Name })
	eff.cluster.name = "cluster"
	for i := range clusterMatched {
		ctp := clusterMatched[i]
		src := fmt.Sprintf("ClusterTokenPolicy/%s", ctp.Name)
		for _, m := range ctp.Spec.Models {
			eff.cluster.rules = append(eff.cluster.rules, sourcedRule{rule: m, source: src})
		}
		if ctp.Spec.Quota != nil {
			eff.Quota = minQuota(eff.Quota, ctp.Spec.Quota)
			sourceSet[src] = struct{}{}
		}
		if ctp.Spec.Enforcement != "" {
			eff.Enforcement = ctp.Spec.Enforcement
		}
		if ctp.Spec.Gateway != nil {
			eff.Gateway = ctp.Spec.Gateway
		}
		if ctp.Spec.DefaultCredentialRef != nil {
			eff.DefaultCredential = ctp.Spec.DefaultCredentialRef.Name
		}
		if len(ctp.Spec.Models) > 0 {
			sourceSet[src] = struct{}{}
		}
	}

	// --- namespace + workload tiers ---
	// Namespace-default policies have an empty selector; workload policies have a selector
	// that matches the supplied pod identity. Policies are applied in Priority order so that
	// the highest-priority policy provides scalar fields (enforcement, gateway).
	nsDefaults := make([]api.TokenPolicy, 0)
	wlMatched := make([]api.TokenPolicy, 0)
	for i := range in.NamespacePolicies {
		tp := in.NamespacePolicies[i]
		if tp.Spec.Selector.IsEmpty() {
			nsDefaults = append(nsDefaults, tp)
			continue
		}
		// Workload-scoped: only relevant when a concrete workload identity is supplied.
		if in.PodLabels == nil && in.ServiceAccount == "" {
			continue
		}
		ok, err := workloadMatches(tp.Spec.Selector, in.PodLabels, in.ServiceAccount)
		if err != nil {
			return nil, fmt.Errorf("TokenPolicy %s/%s selector: %w", tp.Namespace, tp.Name, err)
		}
		if ok {
			wlMatched = append(wlMatched, tp)
		}
	}
	sortByPriority(nsDefaults)
	sortByPriority(wlMatched)

	eff.ns.name = "namespace"
	for i := range nsDefaults {
		tp := nsDefaults[i]
		src := fmt.Sprintf("TokenPolicy/%s/%s", tp.Namespace, tp.Name)
		applyNamespaceTier(eff, &eff.ns, tp, src, sourceSet)
	}
	eff.wl.name = "workload"
	for i := range wlMatched {
		tp := wlMatched[i]
		src := fmt.Sprintf("TokenPolicy/%s/%s", tp.Namespace, tp.Name)
		applyNamespaceTier(eff, &eff.wl, tp, src, sourceSet)
	}

	eff.ModelGoverned = len(eff.cluster.rules) > 0 || len(eff.ns.rules) > 0 || len(eff.wl.rules) > 0
	eff.Governed = len(clusterMatched) > 0 || len(nsDefaults) > 0 || len(wlMatched) > 0

	eff.Sources = make([]string, 0, len(sourceSet))
	for s := range sourceSet {
		eff.Sources = append(eff.Sources, s)
	}
	sort.Strings(eff.Sources)
	return eff, nil
}

// applyNamespaceTier folds a single TokenPolicy into a tier and the scalar fields of eff.
func applyNamespaceTier(eff *Effective, t *tier, tp api.TokenPolicy, src string, sourceSet map[string]struct{}) {
	for _, m := range tp.Spec.Models {
		t.rules = append(t.rules, sourcedRule{rule: m, source: src})
	}
	if len(tp.Spec.Models) > 0 {
		sourceSet[src] = struct{}{}
	}
	if tp.Spec.Quota != nil {
		eff.Quota = minQuota(eff.Quota, tp.Spec.Quota)
		sourceSet[src] = struct{}{}
	}
	if tp.Spec.Enforcement != "" {
		eff.Enforcement = tp.Spec.Enforcement
	}
	if tp.Spec.Gateway != nil {
		eff.Gateway = tp.Spec.Gateway
	}
}

// ResolveForPolicy computes the Effective decision for the scope governed by a specific
// TokenPolicy. It is used by the controller to populate policy status: the target policy is
// placed in its correct tier (namespace-default or workload) while the remaining namespace
// policies and cluster policies provide the inherited context.
func ResolveForPolicy(cluster []api.ClusterTokenPolicy, nsPolicies []api.TokenPolicy, nsLabels map[string]string, target api.TokenPolicy) (*Effective, error) {
	rest := make([]api.TokenPolicy, 0, len(nsPolicies))
	for _, p := range nsPolicies {
		if p.Namespace == target.Namespace && p.Name == target.Name {
			continue
		}
		rest = append(rest, p)
	}

	src := fmt.Sprintf("TokenPolicy/%s/%s", target.Namespace, target.Name)

	if target.Spec.Selector.IsEmpty() {
		// Namespace-default policy: it belongs to the namespace tier, so resolving with it
		// present in NamespacePolicies (and no pod identity) folds it into that tier.
		in := ResolveInput{
			ClusterPolicies:   cluster,
			NamespacePolicies: append(rest, target),
			Namespace:         target.Namespace,
			NamespaceLabels:   nsLabels,
		}
		return Resolve(in)
	}

	// Workload-scoped policy: resolve the cluster + namespace-default base, then fold the
	// target in as the workload tier.
	base, err := Resolve(ResolveInput{
		ClusterPolicies:   cluster,
		NamespacePolicies: rest,
		Namespace:         target.Namespace,
		NamespaceLabels:   nsLabels,
	})
	if err != nil {
		return nil, err
	}
	sset := map[string]struct{}{}
	applyNamespaceTier(base, &base.wl, target, src, sset)
	base.wl.name = "workload"
	base.ModelGoverned = len(base.cluster.rules) > 0 || len(base.ns.rules) > 0 || len(base.wl.rules) > 0
	base.Governed = true
	for s := range sset {
		base.Sources = appendUnique(base.Sources, s)
	}
	sort.Strings(base.Sources)
	return base, nil
}

func appendUnique(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

// Permit evaluates a single provider/model against the resolved hierarchy.
func (e *Effective) Permit(provider, model string) ModelDecision {
	dc, ccred, csrc := evalTier(e.cluster, provider, model)
	dn, ncred, nsrc := evalTier(e.ns, provider, model)
	dw, wcred, wsrc := evalTier(e.wl, provider, model)

	res := ModelDecision{Provider: provider, Model: model}

	// Any explicit/by-omission Deny wins. Report the narrowest denier for a useful message.
	if dw == Deny {
		res.DeniedBy, res.Reason = wsrc, denyReason("workload", wsrc, provider, model)
		return res
	}
	if dn == Deny {
		res.DeniedBy, res.Reason = nsrc, denyReason("namespace", nsrc, provider, model)
		return res
	}
	if dc == Deny {
		res.DeniedBy, res.Reason = csrc, denyReason("cluster", csrc, provider, model)
		return res
	}

	// No denials: the model is permitted (either explicitly allowed or ungoverned).
	res.Allowed = true
	// Credential: most specific Allow that named one wins, else cluster default.
	switch {
	case dw == Allow && wcred != "":
		res.Credential = wcred
	case dn == Allow && ncred != "":
		res.Credential = ncred
	case dc == Allow && ccred != "":
		res.Credential = ccred
	default:
		res.Credential = e.DefaultCredential
	}
	if dc == Abstain && dn == Abstain && dw == Abstain {
		res.Reason = "no model allowlist configured for scope"
	} else {
		res.Reason = "permitted by policy"
	}
	return res
}

// EffectiveModels enumerates every distinct provider/model named by an Allow rule across
// the hierarchy and reports its resolved verdict. Useful for populating policy status.
func (e *Effective) EffectiveModels() []api.EffectiveModel {
	type key struct{ p, m string }
	seen := map[key]string{} // key -> source of first Allow
	order := make([]key, 0)
	collect := func(t tier) {
		for _, r := range t.rules {
			act := normAction(r.rule.Action)
			if act != api.ActionAllow {
				continue
			}
			k := key{strings.ToLower(r.rule.Provider), strings.ToLower(r.rule.Model)}
			if _, ok := seen[k]; !ok {
				seen[k] = r.source
				order = append(order, k)
			}
		}
	}
	collect(e.cluster)
	collect(e.ns)
	collect(e.wl)

	out := make([]api.EffectiveModel, 0, len(order))
	for _, k := range order {
		d := e.Permit(k.p, k.m)
		action := api.ActionAllow
		if !d.Allowed {
			action = api.ActionDeny
		}
		out = append(out, api.EffectiveModel{
			Provider:   k.p,
			Model:      k.m,
			Action:     action,
			Credential: d.Credential,
			Source:     seen[k],
		})
	}
	return out
}

// evalTier returns the tier's decision plus the credential and source of a matching Allow.
func evalTier(t tier, provider, model string) (Decision, string, string) {
	if len(t.rules) == 0 {
		return Abstain, "", ""
	}
	hasAllow := false
	for _, r := range t.rules {
		if normAction(r.rule.Action) == api.ActionAllow {
			hasAllow = true
			break
		}
	}
	// Explicit deny anywhere in the tier wins.
	for _, r := range t.rules {
		if normAction(r.rule.Action) == api.ActionDeny && modelMatches(r.rule, provider, model) {
			return Deny, "", r.source
		}
	}
	// First matching Allow (rules are pre-ordered by Priority) supplies the credential.
	for _, r := range t.rules {
		if normAction(r.rule.Action) == api.ActionAllow && modelMatches(r.rule, provider, model) {
			cred := ""
			if r.rule.CredentialRef != nil {
				cred = r.rule.CredentialRef.Name
			}
			return Allow, cred, r.source
		}
	}
	if hasAllow {
		// Tier publishes an allowlist that this model is not on: deny-by-omission.
		return Deny, "", t.name + " allowlist"
	}
	// Pure denylist that did not match: permit.
	return Allow, "", ""
}

func denyReason(level, source, provider, model string) string {
	if strings.HasSuffix(source, "allowlist") {
		return fmt.Sprintf("%s/%s is not on the %s allowlist", provider, model, level)
	}
	return fmt.Sprintf("%s/%s is denied by %s (%s)", provider, model, level, source)
}

// modelMatches reports whether a rule matches the provider/model, with glob support and
// case-insensitive comparison.
func modelMatches(r api.ModelPermission, provider, model string) bool {
	return globMatch(r.Provider, provider) && globMatch(r.Model, model)
}

func globMatch(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "*" || pattern == value {
		return true
	}
	ok, err := path.Match(pattern, value)
	return err == nil && ok
}

func normAction(a api.PermissionAction) api.PermissionAction {
	if a == "" {
		return api.ActionAllow
	}
	return a
}

// minQuota returns the field-wise minimum of two quotas, treating nil fields as "unset".
func minQuota(a, b *api.TokenQuota) *api.TokenQuota {
	if a == nil {
		return b.DeepCopy()
	}
	if b == nil {
		return a.DeepCopy()
	}
	out := a.DeepCopy()
	out.TokensPerMinute = minPtr(a.TokensPerMinute, b.TokensPerMinute)
	out.RequestsPerMinute = minPtr(a.RequestsPerMinute, b.RequestsPerMinute)
	out.TokensPerDay = minPtr(a.TokensPerDay, b.TokensPerDay)
	out.TokensPerMonth = minPtr(a.TokensPerMonth, b.TokensPerMonth)
	return out
}

func minPtr(a, b *int64) *int64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a <= *b:
		return a
	default:
		return b
	}
}

func sortByPriority(ps []api.TokenPolicy) {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Spec.Priority != ps[j].Spec.Priority {
			return ps[i].Spec.Priority < ps[j].Spec.Priority // ascending: highest applied last (wins scalars)
		}
		return ps[i].Name < ps[j].Name
	})
}

func selectorMatches(sel *metav1.LabelSelector, lbls map[string]string) (bool, error) {
	if sel == nil {
		return true, nil
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, err
	}
	return s.Matches(labels.Set(lbls)), nil
}

// NamespaceSelectorMatches reports whether a namespace's labels satisfy a label selector.
// A nil selector matches everything.
func NamespaceSelectorMatches(sel *metav1.LabelSelector, lbls map[string]string) (bool, error) {
	return selectorMatches(sel, lbls)
}

// ValidGlob returns an error if pattern is not a valid path.Match glob.
func ValidGlob(pattern string) error {
	_, err := path.Match(pattern, "probe")
	return err
}

func workloadMatches(sel *api.WorkloadSelector, podLabels map[string]string, sa string) (bool, error) {
	if sel == nil {
		return true, nil
	}
	if len(sel.ServiceAccounts) > 0 {
		found := false
		for _, want := range sel.ServiceAccounts {
			if want == sa {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	if sel.PodSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(sel.PodSelector)
		if err != nil {
			return false, err
		}
		if !s.Matches(labels.Set(podLabels)) {
			return false, nil
		}
	}
	return true, nil
}
