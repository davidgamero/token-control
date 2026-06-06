package webhookv1alpha1

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/metrics"
)

// +kubebuilder:webhook:path=/validate--v1-pod,mutating=false,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create;update,versions=v1,name=vpod.governance.tokencontrol.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod.governance.tokencontrol.io,admissionReviewVersions=v1

// PodValidator gates pod admission on the model declarations a pod carries.
type PodValidator struct {
	Client client.Client
	Config Config
}

var _ admission.CustomValidator = &PodValidator{}

func (v *PodValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *PodValidator) ValidateUpdate(ctx context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *PodValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *PodValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("expected a Pod but got a %T", obj)
	}
	log := logf.FromContext(ctx)
	namespace := namespaceFromCtx(ctx, pod.Namespace)
	if v.Config.exempt(namespace) {
		return nil, nil
	}
	declared, err := declaredModelsForPod(ctx, v.Client, namespace, pod)
	if err != nil {
		log.Error(err, "failed to resolve model claims; allowing pod", "namespace", namespace)
		return nil, nil
	}
	if len(declared) == 0 {
		return nil, nil
	}

	eff, err := resolveEffective(ctx, v.Client, namespace, pod.Labels, saOf(pod))
	if err != nil {
		// Fail open on transient errors: the webhook failurePolicy is Ignore, but be explicit.
		log.Error(err, "failed to resolve effective policy; allowing pod", "namespace", namespace)
		return nil, nil
	}
	if !eff.Governed || eff.Enforcement == api.EnforcementDisabled {
		return nil, nil
	}

	var warnings admission.Warnings
	var violations []string
	for _, dm := range declared {
		dec := eff.Permit(dm.Provider, dm.Model)
		if dec.Allowed {
			continue
		}
		metrics.ModelViolations.WithLabelValues(namespace, dm.Provider, dm.Model, string(eff.Enforcement)).Inc()
		if eff.Enforcement == api.EnforcementEnforce {
			violations = append(violations, dec.Reason)
		} else { // Audit
			warnings = append(warnings, fmt.Sprintf("token-control(audit): %s", dec.Reason))
		}
	}

	if len(violations) > 0 {
		metrics.AdmissionDecisions.WithLabelValues("deny", namespace, string(eff.Enforcement)).Inc()
		return warnings, apierrors.NewForbidden(
			corev1.Resource("pods"), pod.Name,
			fmt.Errorf("token-control: %s", strings.Join(violations, "; ")),
		)
	}
	metrics.AdmissionDecisions.WithLabelValues("allow", namespace, string(eff.Enforcement)).Inc()
	return warnings, nil
}

// PodDefaulter injects the bound provider credential(s) into governed pods.
type PodDefaulter struct {
	Client client.Client
	Config Config
}

var _ admission.CustomDefaulter = &PodDefaulter{}

func (d *PodDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod but got a %T", obj)
	}
	log := logf.FromContext(ctx)
	namespace := namespaceFromCtx(ctx, pod.Namespace)
	if d.Config.exempt(namespace) {
		return nil
	}
	if pod.GetAnnotations()[api.AnnotationInjectionDisabled] == "true" {
		return nil
	}
	declared, err := declaredModelsForPod(ctx, d.Client, namespace, pod)
	if err != nil {
		log.Error(err, "failed to resolve model claims; skipping injection", "namespace", namespace)
		return nil
	}
	if len(declared) == 0 {
		return nil
	}

	eff, err := resolveEffective(ctx, d.Client, namespace, pod.Labels, saOf(pod))
	if err != nil {
		log.Error(err, "failed to resolve effective policy; skipping injection", "namespace", namespace)
		return nil
	}
	if !eff.Governed {
		return nil
	}

	// Determine the ordered, unique set of credentials to bind for allowed declared models.
	// The resolved policy credential wins; a ModelClaim's per-model credential preference is
	// the fallback when the hierarchy permits the model but binds no credential of its own.
	var credOrder []string
	seen := map[string]bool{}
	for _, dm := range declared {
		dec := eff.Permit(dm.Provider, dm.Model)
		if !dec.Allowed {
			continue
		}
		cred := dec.Credential
		if cred == "" {
			cred = dm.Credential
		}
		if cred != "" && !seen[cred] {
			seen[cred] = true
			credOrder = append(credOrder, cred)
		}
	}

	var bound []string
	for _, credName := range credOrder {
		var mc api.ModelCredential
		if err := d.Client.Get(ctx, client.ObjectKey{Name: credName}, &mc); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("bound credential not found; skipping", "credential", credName)
				continue
			}
			return err
		}
		authorized, err := credentialAuthorizes(ctx, d.Client, &mc, namespace)
		if err != nil {
			return err
		}
		if !authorized {
			log.Info("namespace not authorized for credential; skipping", "credential", credName, "namespace", namespace)
			continue
		}
		if injectCredential(pod, &mc) {
			bound = append(bound, credName)
			metrics.CredentialsInjected.WithLabelValues(namespace, credName).Inc()
		}
	}

	if len(bound) > 0 {
		setAnnotation(pod, api.AnnotationCredentialsBound, strings.Join(bound, ","))
	}
	if len(eff.Sources) > 0 {
		setAnnotation(pod, api.AnnotationEffectivePolicy, strings.Join(eff.Sources, ","))
	}
	return nil
}

// injectCredential mutates the pod to deliver the credential. It returns true if it changed
// the pod. Existing env vars/volumes with the same name are respected (not overwritten).
func injectCredential(pod *corev1.Pod, mc *api.ModelCredential) bool {
	mode := api.InjectEnv
	if mc.Spec.Injection != nil && mc.Spec.Injection.Mode != "" {
		mode = mc.Spec.Injection.Mode
	}
	secretName := api.ManagedSecretPrefix + mc.Name
	key := mc.Spec.SecretRef.Key

	switch mode {
	case api.InjectNone:
		return false
	case api.InjectProjectedVolume:
		return injectVolume(pod, mc, secretName, key)
	default: // Env
		envName := defaultEnvName(mc.Spec.Provider)
		if mc.Spec.Injection != nil && mc.Spec.Injection.EnvName != "" {
			envName = mc.Spec.Injection.EnvName
		}
		return injectEnv(pod, envName, secretName, key)
	}
}

func injectEnv(pod *corev1.Pod, envName, secretName, key string) bool {
	changed := false
	envVar := corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
	apply := func(cs []corev1.Container) {
		for i := range cs {
			if hasEnv(cs[i].Env, envName) {
				continue
			}
			cs[i].Env = append(cs[i].Env, envVar)
			changed = true
		}
	}
	apply(pod.Spec.Containers)
	apply(pod.Spec.InitContainers)
	return changed
}

func injectVolume(pod *corev1.Pod, mc *api.ModelCredential, secretName, key string) bool {
	volName := "tc-cred-" + mc.Name
	mountPath := "/var/run/secrets/tokencontrol/" + mc.Spec.Provider
	if mc.Spec.Injection != nil && mc.Spec.Injection.MountPath != "" {
		mountPath = mc.Spec.Injection.MountPath
	}
	if !hasVolume(pod.Spec.Volumes, volName) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
					Items:      []corev1.KeyToPath{{Key: key, Path: "apikey"}},
				},
			},
		})
	}
	mount := corev1.VolumeMount{Name: volName, MountPath: mountPath, ReadOnly: true}
	changed := false
	apply := func(cs []corev1.Container) {
		for i := range cs {
			if hasMount(cs[i].VolumeMounts, volName) {
				continue
			}
			cs[i].VolumeMounts = append(cs[i].VolumeMounts, mount)
			changed = true
		}
	}
	apply(pod.Spec.Containers)
	apply(pod.Spec.InitContainers)
	return changed
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func hasVolume(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

func hasMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}
