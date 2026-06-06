// Command manager runs the token-control governance controller and admission webhooks.
package main

import (
	"flag"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	api "github.com/token-control/token-control/api/v1alpha1"
	"github.com/token-control/token-control/internal/controller"
	webhookv1alpha1 "github.com/token-control/token-control/internal/webhook/v1alpha1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		webhookPort          int
		certDir              string
		enableWebhooks       bool
		operatorNamespace    string
		exemptNamespaces     string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the admission webhook server binds to.")
	flag.StringVar(&certDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory holding the webhook serving certificate (tls.crt/tls.key).")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true, "Enable the admission webhook server.")
	flag.StringVar(&operatorNamespace, "operator-namespace", envOr("POD_NAMESPACE", "token-control-system"), "Namespace the controller runs in (always exempt; default for credential SecretRefs).")
	flag.StringVar(&exemptNamespaces, "exempt-namespaces", "kube-system,kube-public,kube-node-lease", "Comma-separated namespaces exempt from pod governance.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var webhookServer webhook.Server
	if enableWebhooks {
		webhookServer = webhook.NewServer(webhook.Options{Port: webhookPort, CertDir: certDir})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "token-control.tokencontrol.io",
		WebhookServer:          webhookServer,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Uncached client for Secret IO and unstructured gateway artifacts, so the manager does
	// not start informers for every Secret or for gateway CRDs that may not be installed.
	uncached, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		setupLog.Error(err, "unable to build uncached client")
		os.Exit(1)
	}

	if err := (&controller.TokenPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Apply:  uncached,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TokenPolicy")
		os.Exit(1)
	}
	if err := (&controller.ClusterTokenPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterTokenPolicy")
		os.Exit(1)
	}
	if err := (&controller.ModelCredentialReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		SecretClient:      uncached,
		OperatorNamespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelCredential")
		os.Exit(1)
	}
	if err := (&controller.ModelClaimReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelClaim")
		os.Exit(1)
	}

	if enableWebhooks {
		cfg := webhookv1alpha1.Config{
			OperatorNamespace: operatorNamespace,
			ExemptNamespaces:  parseSet(exemptNamespaces),
		}
		if err := webhookv1alpha1.SetupWebhooksWithManager(mgr, cfg); err != nil {
			setupLog.Error(err, "unable to set up webhooks")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	readyz := healthz.Ping
	if enableWebhooks {
		readyz = mgr.GetWebhookServer().StartedChecker()
	}
	if err := mgr.AddReadyzCheck("readyz", readyz); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting token-control manager", "operatorNamespace", operatorNamespace, "webhooks", enableWebhooks)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseSet(csv string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}
