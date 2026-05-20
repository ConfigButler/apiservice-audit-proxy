package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientgotransport "k8s.io/client-go/transport"

	"github.com/ConfigButler/apiservice-audit-proxy/pkg/identity"
	auditproxy "github.com/ConfigButler/apiservice-audit-proxy/pkg/proxy"
	"github.com/ConfigButler/apiservice-audit-proxy/pkg/webhook"
)

const (
	defaultListenAddress    = ":9445"
	defaultReadTimeout      = 15 * time.Second
	defaultWriteTimeout     = 30 * time.Second
	defaultIdleTimeout      = 60 * time.Second
	defaultShutdownTimeout  = 10 * time.Second
	defaultWebhookTimeout   = 5 * time.Second
	defaultMaxAuditBodySize = int64(1024 * 1024)

	// requestHeaderTrustStartupTimeout bounds the strict initial load of
	// requestheader trust from the cluster ConfigMap. If no usable trust
	// snapshot is available within this window, startup fails.
	requestHeaderTrustStartupTimeout = 30 * time.Second
	// requestHeaderTrustPollInterval is how often the strict startup gate
	// rechecks for a usable trust snapshot.
	requestHeaderTrustPollInterval = 100 * time.Millisecond

	backendIdentityModeRequestHeader = "requestheader"
	backendIdentityModeImpersonation = "impersonation"

	defaultBackendIdentityMode = backendIdentityModeRequestHeader
	//nolint:gosec // G101: this is the standard projected ServiceAccount token file path, not a credential.
	defaultBackendImpersonationTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

	exitCodeUsage = 2
)

type config struct {
	listenAddress                        string
	backendURL                           string
	backendInsecureSkipVerify            bool
	backendCAFile                        string
	backendClientCertFile                string
	backendClientKeyFile                 string
	backendServerName                    string
	backendIdentityMode                  string
	backendImpersonationTokenFile        string
	backendImpersonationExtraKeys        string
	backendImpersonationForwardAllExtras bool
	backendImpersonationForwardUID       bool
	webhookKubeconfig                    string
	webhookTimeout                       time.Duration
	maxAuditBodyBytes                    int64
	captureTempDir                       string
	tlsCertFile                          string
	tlsPrivateKeyFile                    string
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		logger.Error("invalid flags", "error", err)
		os.Exit(exitCodeUsage)
	}

	backendURL, err := url.Parse(cfg.backendURL)
	if err != nil {
		logger.Error("invalid backend URL", "error", err)
		os.Exit(1)
	}

	backendTransport, err := buildBackendTransport(backendURL, cfg)
	if err != nil {
		logger.Error("unable to configure backend transport", "error", err)
		os.Exit(1)
	}

	var roundTripper http.RoundTripper = backendTransport
	var backendIdentity auditproxy.BackendIdentity = auditproxy.RequestHeaderForwarder{}
	if cfg.backendIdentityMode == backendIdentityModeImpersonation {
		roundTripper, err = wrapImpersonationTransport(backendTransport, cfg.backendImpersonationTokenFile)
		if err != nil {
			logger.Error("unable to configure backend impersonation transport", "error", err)
			os.Exit(1)
		}
		backendIdentity = auditproxy.NewImpersonator(auditproxy.ImpersonatorConfig{
			ExtraKeys:        splitCommaList(cfg.backendImpersonationExtraKeys),
			ForwardAllExtras: cfg.backendImpersonationForwardAllExtras,
			ForwardUID:       cfg.backendImpersonationForwardUID,
		})
	}

	// stop is intentionally not deferred: the fatal paths below call os.Exit,
	// which would skip the defer anyway. It is released on the shutdown path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	kubeClient, err := buildKubeClient()
	if err != nil {
		logger.Error("unable to build in-cluster Kubernetes client", "error", err)
		os.Exit(1)
	}

	identityExtractor, trustController, err := identity.NewClusterExtractor(kubeClient)
	if err != nil {
		logger.Error("unable to initialize cluster requestheader identity extractor", "error", err)
		os.Exit(1)
	}

	// Strict startup gate: the proxy must not serve until it has a usable,
	// cluster-sourced trust snapshot. There is no unverified fallback.
	if err := startRequestHeaderTrust(ctx, logger, kubeClient, trustController); err != nil {
		logger.Error("unable to establish requestheader trust from cluster", "error", err)
		os.Exit(1)
	}

	webhookClient, err := webhook.NewClientFromKubeconfig(cfg.webhookKubeconfig, cfg.webhookTimeout)
	if err != nil {
		logger.Error("unable to initialize webhook client", "error", err)
		os.Exit(1)
	}
	logger.Info("audit webhook client configured",
		"endpoint", webhookEndpointString(webhookClient),
		"timeout", cfg.webhookTimeout,
		"kubeconfig", cfg.webhookKubeconfig,
	)

	handler, err := auditproxy.NewHandler(auditproxy.HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     webhookClient,
		IdentityExtractor: identityExtractor,
		Logger:            logger.With("component", "proxy"),
		Transport:         roundTripper,
		BackendIdentity:   backendIdentity,
		MaxAuditBodyBytes: cfg.maxAuditBodyBytes,
		TempDir:           cfg.captureTempDir,
	})
	if err != nil {
		logger.Error("unable to initialize proxy handler", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/livez", http.HandlerFunc(handleHealth))
	mux.Handle("/readyz", readinessHandler(trustController))
	mux.Handle("/", handler)

	server := &http.Server{
		Addr:         cfg.listenAddress,
		Handler:      mux,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
	}
	server.TLSConfig = buildServingTLSConfig(cfg)

	logStartupAssumptions(logger, cfg, backendURL)

	go func() {
		<-ctx.Done()
		stop()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("audit pass-through API server listening; waiting for first request from the cluster",
		"address", cfg.listenAddress,
		"tls_enabled", cfg.tlsCertFile != "",
	)
	if err := serve(server, cfg); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// logStartupAssumptions writes one log line per subsystem so an operator can
// see, in order, exactly what choices the proxy made. Insecure or wide-open
// settings are promoted to Warn so they are not lost in the Info noise. Each
// line is one-shot — there is no per-request logging.
func logStartupAssumptions(logger *slog.Logger, cfg config, backendURL *url.URL) {
	logger.Info("backend wiring",
		"backend_url", backendURL.String(),
		"backend_identity_mode", cfg.backendIdentityMode,
		"backend_ca_file", cfg.backendCAFile,
		"backend_server_name", cfg.backendServerName,
	)
	if cfg.backendInsecureSkipVerify {
		logger.Warn("backend TLS verification is disabled (--backend-insecure-skip-verify); " +
			"backend identity is not authenticated — only acceptable for prototype clusters")
	}

	switch cfg.backendIdentityMode {
	case backendIdentityModeImpersonation:
		logger.Info("backend identity: impersonation",
			"token_file", cfg.backendImpersonationTokenFile,
			"forward_uid", cfg.backendImpersonationForwardUID,
			"forward_all_extras", cfg.backendImpersonationForwardAllExtras,
			"extra_keys_allowlist", cfg.backendImpersonationExtraKeys,
		)
		if cfg.backendImpersonationForwardAllExtras {
			logger.Warn("impersonation forwards ALL inbound extras to the backend " +
				"(--backend-impersonation-forward-all-extras); any caller-supplied extra key reaches the backend")
		}
	case backendIdentityModeRequestHeader:
		logger.Info("backend identity: requestheader (forwarding inbound X-Remote-* headers unchanged)")
	}

	logger.Info("inbound trust source",
		"configmap", fmt.Sprintf("%s/%s",
			identity.AuthenticationConfigMapNamespace, identity.AuthenticationConfigMapName),
	)

	logger.Info("audit capture",
		"max_audit_body_bytes", cfg.maxAuditBodyBytes,
		"capture_temp_dir", cfg.captureTempDir,
	)

	if cfg.tlsCertFile == "" {
		logger.Warn("serving plain HTTP (no --tls-cert-file); " +
			"requestheader identity is meaningless without TLS — only acceptable for local debugging")
	} else {
		logger.Info("serving TLS",
			"cert_file", cfg.tlsCertFile,
			"private_key_file", cfg.tlsPrivateKeyFile,
		)
	}
}

// webhookEndpointString returns the configured webhook destination as a string
// for startup logging. Returns "<unknown>" when the client did not expose an
// endpoint (e.g. test doubles).
func webhookEndpointString(client *webhook.Client) string {
	if client == nil {
		return "<unknown>"
	}
	endpoint := client.Endpoint()
	if endpoint == nil {
		return "<unknown>"
	}
	return endpoint.String()
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	cfg := config{}

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.listenAddress, "listen-address", defaultListenAddress, "Address for the pass-through server.")
	fs.StringVar(&cfg.backendURL, "backend-url", "", "URL of the real aggregated backend.")
	fs.BoolVar(
		&cfg.backendInsecureSkipVerify,
		"backend-insecure-skip-verify",
		false,
		"Skip TLS verification for an HTTPS backend. Intended only for prototype cluster wiring.",
	)
	fs.StringVar(
		&cfg.backendCAFile,
		"backend-ca-file",
		"",
		"PEM bundle used to verify an HTTPS backend certificate.",
	)
	fs.StringVar(
		&cfg.backendClientCertFile,
		"backend-client-cert-file",
		"",
		"Client certificate file used for mTLS when the HTTPS backend requires caller authentication.",
	)
	fs.StringVar(
		&cfg.backendClientKeyFile,
		"backend-client-key-file",
		"",
		"Client private key file used for mTLS when the HTTPS backend requires caller authentication.",
	)
	fs.StringVar(
		&cfg.backendServerName,
		"backend-server-name",
		"",
		"Optional TLS server name override for HTTPS backend verification.",
	)
	fs.StringVar(
		&cfg.backendIdentityMode,
		"backend-identity-mode",
		defaultBackendIdentityMode,
		"Identity the proxy presents to the backend: requestheader (forward X-Remote-*) or impersonation.",
	)
	fs.StringVar(
		&cfg.backendImpersonationTokenFile,
		"backend-impersonation-token-file",
		defaultBackendImpersonationTokenFile,
		"ServiceAccount token file used as the backend bearer token in impersonation mode.",
	)
	fs.StringVar(
		&cfg.backendImpersonationExtraKeys,
		"backend-impersonation-extra-keys",
		"",
		"Comma-separated allowlist of decoded extra keys projected into Impersonate-Extra-* headers.",
	)
	fs.BoolVar(
		&cfg.backendImpersonationForwardAllExtras,
		"backend-impersonation-forward-all-extras",
		false,
		"Project every inbound extra into Impersonate-Extra-*. Mutually exclusive with --backend-impersonation-extra-keys.",
	)
	fs.BoolVar(
		&cfg.backendImpersonationForwardUID,
		"backend-impersonation-forward-uid",
		true,
		"Project Impersonate-Uid when the verified identity has a non-empty UID.",
	)
	fs.StringVar(
		&cfg.webhookKubeconfig,
		"webhook-kubeconfig",
		"",
		"Kubeconfig-style client config used for outbound audit webhook delivery.",
	)
	fs.DurationVar(
		&cfg.webhookTimeout,
		"webhook-timeout",
		defaultWebhookTimeout,
		"HTTP timeout for best-effort audit webhook delivery.",
	)
	fs.Int64Var(
		&cfg.maxAuditBodyBytes,
		"max-audit-body-bytes",
		defaultMaxAuditBodySize,
		"Maximum body size captured into audit requestObject and responseObject.",
	)
	fs.StringVar(
		&cfg.captureTempDir,
		"capture-temp-dir",
		"",
		"Directory used for temporary request/response body spooling during audited proxying.",
	)
	fs.StringVar(&cfg.tlsCertFile, "tls-cert-file", "", "Serving certificate file for inbound HTTPS.")
	fs.StringVar(&cfg.tlsPrivateKeyFile, "tls-private-key-file", "", "Serving private key file for inbound HTTPS.")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if cfg.backendURL == "" {
		fs.Usage()
		return config{}, errors.New("--backend-url is required")
	}
	if cfg.webhookKubeconfig == "" {
		fs.Usage()
		return config{}, errors.New("--webhook-kubeconfig is required")
	}
	if cfg.maxAuditBodyBytes <= 0 {
		fs.Usage()
		return config{}, errors.New("--max-audit-body-bytes must be greater than zero")
	}
	if err := validateServingTLSFlags(cfg); err != nil {
		fs.Usage()
		return config{}, err
	}
	if err := validateBackendClientTLSFlags(cfg); err != nil {
		fs.Usage()
		return config{}, err
	}
	if err := validateBackendIdentityFlags(cfg, setFlags); err != nil {
		fs.Usage()
		return config{}, err
	}

	return cfg, nil
}

// validateBackendIdentityFlags enforces the backend identity mode rules.
//
// Inbound requestheader trust is always cluster-sourced and always verified, so
// impersonation mode no longer asserts any inbound trust flags. Backend client
// certificate flags are rejected in impersonation mode to keep the identity
// model unambiguous in this first implementation.
func validateBackendIdentityFlags(cfg config, setFlags map[string]bool) error {
	switch cfg.backendIdentityMode {
	case backendIdentityModeRequestHeader, backendIdentityModeImpersonation:
	default:
		return fmt.Errorf("--backend-identity-mode must be %q or %q",
			backendIdentityModeRequestHeader, backendIdentityModeImpersonation)
	}

	impersonationOnlyFlags := []string{
		"backend-impersonation-token-file",
		"backend-impersonation-extra-keys",
		"backend-impersonation-forward-all-extras",
		"backend-impersonation-forward-uid",
	}

	if cfg.backendIdentityMode == backendIdentityModeRequestHeader {
		for _, name := range impersonationOnlyFlags {
			if setFlags[name] {
				return fmt.Errorf("--%s requires --backend-identity-mode=impersonation", name)
			}
		}
		return nil
	}

	if cfg.backendClientCertFile != "" || cfg.backendClientKeyFile != "" {
		return errors.New(
			"--backend-client-cert-file and --backend-client-key-file are not yet supported " +
				"with --backend-identity-mode=impersonation",
		)
	}
	if cfg.backendImpersonationExtraKeys != "" && cfg.backendImpersonationForwardAllExtras {
		return errors.New(
			"--backend-impersonation-extra-keys and --backend-impersonation-forward-all-extras are mutually exclusive",
		)
	}

	return nil
}

// splitCommaList splits a comma-separated flag value, trimming whitespace and
// dropping empty entries. An empty or whitespace-only value yields nil.
func splitCommaList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}

	return result
}

// wrapImpersonationTransport wraps the backend transport with a refreshing
// bearer-token round tripper. The token file is read once during construction;
// a missing or unreadable file is therefore a startup failure.
func wrapImpersonationTransport(base http.RoundTripper, tokenFile string) (http.RoundTripper, error) {
	roundTripper, err := clientgotransport.NewBearerAuthWithRefreshRoundTripper("", tokenFile, base)
	if err != nil {
		return nil, fmt.Errorf("configure backend impersonation bearer token: %w", err)
	}

	return roundTripper, nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readinessHandler reports ready only once the proxy holds a usable,
// cluster-sourced requestheader trust snapshot. A transient loss of the watch
// does not flip it back: the controller retains last-known-good trust.
func readinessHandler(controller *identity.RequestHeaderTrustController) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !controller.Ready() {
			http.Error(w, "requestheader trust not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// buildKubeClient builds an in-cluster Kubernetes client. The proxy runs as a
// ServiceAccount and reads its inbound trust from a cluster ConfigMap, so a
// working in-cluster client is a hard startup prerequisite.
func buildKubeClient() (kubernetes.Interface, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("build in-cluster Kubernetes client config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build in-cluster Kubernetes client: %w", err)
	}

	return client, nil
}

// startRequestHeaderTrust performs the strict initial trust load and starts the
// background watch. It fails loudly if the proxy cannot establish a usable,
// cluster-sourced trust snapshot: there is no unverified fallback.
func startRequestHeaderTrust(
	ctx context.Context,
	logger *slog.Logger,
	client kubernetes.Interface,
	controller *identity.RequestHeaderTrustController,
) error {
	startupCtx, cancel := context.WithTimeout(ctx, requestHeaderTrustStartupTimeout)
	defer cancel()

	// Explicit Get for diagnostics: the dynamic controllers swallow CA load
	// errors, so a bare Get is what surfaces *why* trust is missing — a missing
	// RoleBinding (Forbidden) versus an absent ConfigMap (NotFound).
	configMapRef := fmt.Sprintf("%s/%s",
		identity.AuthenticationConfigMapNamespace, identity.AuthenticationConfigMapName)
	logger.Info("loading requestheader trust from cluster ConfigMap",
		"configmap", configMapRef,
		"timeout", requestHeaderTrustStartupTimeout,
	)
	_, err := client.CoreV1().ConfigMaps(identity.AuthenticationConfigMapNamespace).
		Get(startupCtx, identity.AuthenticationConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsForbidden(err):
		return fmt.Errorf("cannot read %s: the proxy ServiceAccount needs a RoleBinding to the "+
			"kube-system extension-apiserver-authentication-reader Role: %w", configMapRef, err)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s not found: the cluster does not publish requestheader trust: %w",
			configMapRef, err)
	case err != nil:
		return fmt.Errorf("read %s: %w", configMapRef, err)
	}

	// RunOnce surfaces denied RBAC and malformed requestheader header JSON
	// immediately, before the watch is even started.
	if err := controller.RunOnce(startupCtx); err != nil {
		return fmt.Errorf("initial requestheader trust load from %s: %w", configMapRef, err)
	}

	// Start the watch so the CA bundle — loaded via the informer — can populate
	// and so later rotation is adopted without a restart.
	go controller.Run(ctx, 1)

	// Gate on a usable trust snapshot: a parsed CA bundle and a non-empty
	// username-header list. Without it the proxy cannot verify anyone.
	if err := waitForTrust(startupCtx, controller); err != nil {
		return fmt.Errorf("requestheader trust from %s never became usable "+
			"(missing CA bundle, username headers, or RBAC): %w", configMapRef, err)
	}

	logger.Info("requestheader trust loaded from cluster ConfigMap", "configmap", configMapRef)
	return nil
}

// waitForTrust blocks until the controller reports a usable trust snapshot or
// ctx expires.
func waitForTrust(ctx context.Context, controller *identity.RequestHeaderTrustController) error {
	ticker := time.NewTicker(requestHeaderTrustPollInterval)
	defer ticker.Stop()

	for {
		if controller.Ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func serve(server *http.Server, cfg config) error {
	if cfg.tlsCertFile == "" {
		return server.ListenAndServe()
	}

	return server.ListenAndServeTLS(cfg.tlsCertFile, cfg.tlsPrivateKeyFile)
}

func validateServingTLSFlags(cfg config) error {
	if (cfg.tlsCertFile != "") == (cfg.tlsPrivateKeyFile != "") {
		return nil
	}

	return errors.New("--tls-cert-file and --tls-private-key-file must be provided together")
}

// buildServingTLSConfig configures the inbound serving TLS.
//
// ClientAuth is tls.RequestClientCert: the TLS layer asks for a client
// certificate but does not itself verify the chain. The requestheader x509
// verifier — backed by the controller's dynamic, cluster-sourced CA — is the
// single inbound trust authority, so there is no static ClientCAs pool to drift
// out of sync. A certless connection still completes the handshake; a certless
// or untrusted-cert request simply carries no verified identity and is rejected
// by the handler.
func buildServingTLSConfig(cfg config) *tls.Config {
	if cfg.tlsCertFile == "" {
		return nil
	}

	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequestClientCert,
	}
}

func validateBackendClientTLSFlags(cfg config) error {
	hasCert := cfg.backendClientCertFile != ""
	hasKey := cfg.backendClientKeyFile != ""
	if hasCert == hasKey {
		return nil
	}

	return errors.New("--backend-client-cert-file and --backend-client-key-file must be provided together")
}

func buildBackendTransport(backendURL *url.URL, cfg config) (*http.Transport, error) {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default transport is %T; expected *http.Transport", http.DefaultTransport)
	}

	transport := baseTransport.Clone()
	if backendURL == nil {
		return nil, errors.New("backend URL is required")
	}
	if backendURL.Scheme != "http" && backendURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported --backend-url scheme %q", backendURL.Scheme)
	}

	if backendURL.Scheme != "https" {
		if cfg.backendInsecureSkipVerify || cfg.backendCAFile != "" || cfg.backendServerName != "" ||
			cfg.backendClientCertFile != "" || cfg.backendClientKeyFile != "" {
			return nil, errors.New("backend TLS flags require an https --backend-url")
		}

		return transport, nil
	}

	if cfg.backendInsecureSkipVerify && cfg.backendCAFile != "" {
		return nil, errors.New("--backend-insecure-skip-verify and --backend-ca-file are mutually exclusive")
	}
	if !cfg.backendInsecureSkipVerify && cfg.backendCAFile == "" {
		return nil, errors.New("https --backend-url requires --backend-insecure-skip-verify or --backend-ca-file")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.backendInsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if cfg.backendServerName != "" {
		tlsConfig.ServerName = cfg.backendServerName
	}
	if cfg.backendClientCertFile != "" {
		certificate, err := loadKeyPair(cfg.backendClientCertFile, cfg.backendClientKeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	if cfg.backendCAFile != "" {
		rootCAs, err := loadCertPool(cfg.backendCAFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = rootCAs
	}

	transport.TLSClientConfig = tlsConfig
	return transport, nil
}

func loadKeyPair(certPath, keyPath string) (tls.Certificate, error) {
	certificate, err := tls.LoadX509KeyPair(filepath.Clean(certPath), filepath.Clean(keyPath))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load backend client certificate: %w", err)
	}

	return certificate, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system cert pool: %w", err)
	}
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	pemBytes, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read backend CA file: %w", err)
	}
	if !rootCAs.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("parse backend CA file: no certificates found")
	}

	return rootCAs, nil
}
