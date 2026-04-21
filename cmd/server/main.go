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
	"syscall"
	"time"

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
)

type config struct {
	listenAddress             string
	backendURL                string
	backendInsecureSkipVerify bool
	backendCAFile             string
	backendClientCertFile     string
	backendClientKeyFile      string
	backendServerName         string
	clientCAFile              string
	webhookKubeconfig         string
	webhookTimeout            time.Duration
	maxAuditBodyBytes         int64
	captureTempDir            string
	tlsCertFile               string
	tlsPrivateKeyFile         string
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		logger.Error("invalid flags", "error", err)
		os.Exit(2)
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
	identityExtractor, err := identity.NewExtractor(cfg.clientCAFile)
	if err != nil {
		logger.Error("unable to initialize requestheader identity extractor", "error", err)
		os.Exit(1)
	}

	webhookClient, err := webhook.NewClientFromKubeconfig(cfg.webhookKubeconfig, cfg.webhookTimeout)
	if err != nil {
		logger.Error("unable to initialize webhook client", "error", err)
		os.Exit(1)
	}

	handler, err := auditproxy.NewHandler(auditproxy.HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     webhookClient,
		IdentityExtractor: identityExtractor,
		Logger:            logger.With("component", "proxy"),
		Transport:         backendTransport,
		MaxAuditBodyBytes: cfg.maxAuditBodyBytes,
		TempDir:           cfg.captureTempDir,
	})
	if err != nil {
		logger.Error("unable to initialize proxy handler", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/livez", http.HandlerFunc(handleHealth))
	mux.Handle("/readyz", http.HandlerFunc(handleHealth))
	mux.Handle("/", handler)

	server := &http.Server{
		Addr:         cfg.listenAddress,
		Handler:      mux,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
	}
	server.TLSConfig, err = buildServingTLSConfig(cfg)
	if err != nil {
		logger.Error("unable to configure serving TLS", "error", err)
		os.Exit(1)
	}

	logger.Info(
		"starting audit pass-through API server prototype",
		"listen_address", cfg.listenAddress,
		"backend_url", backendURL.String(),
		"backend_insecure_skip_verify", cfg.backendInsecureSkipVerify,
		"backend_ca_file", cfg.backendCAFile,
		"backend_client_cert_file", cfg.backendClientCertFile,
		"backend_client_key_file", cfg.backendClientKeyFile,
		"backend_server_name", cfg.backendServerName,
		"client_ca_file", cfg.clientCAFile,
		"webhook_kubeconfig", cfg.webhookKubeconfig,
		"max_audit_body_bytes", cfg.maxAuditBodyBytes,
		"capture_temp_dir", cfg.captureTempDir,
		"tls_enabled", cfg.tlsCertFile != "",
		"tls_cert_file", cfg.tlsCertFile,
		"tls_private_key_file", cfg.tlsPrivateKeyFile,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	if err := serve(server, cfg); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
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
		&cfg.clientCAFile,
		"client-ca-file",
		"",
		"PEM bundle used to verify the front-proxy client certificate before trusting delegated X-Remote-* headers.",
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

	if cfg.backendURL == "" {
		fs.Usage()
		return config{}, fmt.Errorf("--backend-url is required")
	}
	if cfg.webhookKubeconfig == "" {
		fs.Usage()
		return config{}, fmt.Errorf("--webhook-kubeconfig is required")
	}
	if cfg.maxAuditBodyBytes <= 0 {
		fs.Usage()
		return config{}, fmt.Errorf("--max-audit-body-bytes must be greater than zero")
	}
	if err := validateServingTLSFlags(cfg); err != nil {
		fs.Usage()
		return config{}, err
	}
	if err := validateBackendClientTLSFlags(cfg); err != nil {
		fs.Usage()
		return config{}, err
	}

	return cfg, nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func serve(server *http.Server, cfg config) error {
	if cfg.tlsCertFile == "" {
		return server.ListenAndServe()
	}

	return server.ListenAndServeTLS(cfg.tlsCertFile, cfg.tlsPrivateKeyFile)
}

func validateServingTLSFlags(cfg config) error {
	hasCert := cfg.tlsCertFile != ""
	hasKey := cfg.tlsPrivateKeyFile != ""
	if hasCert == hasKey {
		if cfg.clientCAFile != "" && !hasCert {
			return fmt.Errorf("--client-ca-file requires --tls-cert-file and --tls-private-key-file")
		}
		return nil
	}

	return fmt.Errorf("--tls-cert-file and --tls-private-key-file must be provided together")
}

func buildServingTLSConfig(cfg config) (*tls.Config, error) {
	if cfg.tlsCertFile == "" {
		return nil, nil
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.clientCAFile == "" {
		return tlsConfig, nil
	}

	clientCAs, err := loadStaticCertPool(cfg.clientCAFile, "client CA")
	if err != nil {
		return nil, err
	}

	tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
	tlsConfig.ClientCAs = clientCAs
	return tlsConfig, nil
}

func validateBackendClientTLSFlags(cfg config) error {
	hasCert := cfg.backendClientCertFile != ""
	hasKey := cfg.backendClientKeyFile != ""
	if hasCert == hasKey {
		return nil
	}

	return fmt.Errorf("--backend-client-cert-file and --backend-client-key-file must be provided together")
}

func buildBackendTransport(backendURL *url.URL, cfg config) (*http.Transport, error) {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default transport is %T; expected *http.Transport", http.DefaultTransport)
	}

	transport := baseTransport.Clone()
	if backendURL == nil {
		return nil, fmt.Errorf("backend URL is required")
	}
	if backendURL.Scheme != "http" && backendURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported --backend-url scheme %q", backendURL.Scheme)
	}

	if backendURL.Scheme != "https" {
		if cfg.backendInsecureSkipVerify || cfg.backendCAFile != "" || cfg.backendServerName != "" ||
			cfg.backendClientCertFile != "" || cfg.backendClientKeyFile != "" {
			return nil, fmt.Errorf("backend TLS flags require an https --backend-url")
		}

		return transport, nil
	}

	if cfg.backendInsecureSkipVerify && cfg.backendCAFile != "" {
		return nil, fmt.Errorf("--backend-insecure-skip-verify and --backend-ca-file are mutually exclusive")
	}
	if !cfg.backendInsecureSkipVerify && cfg.backendCAFile == "" {
		return nil, fmt.Errorf("https --backend-url requires --backend-insecure-skip-verify or --backend-ca-file")
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

func loadStaticCertPool(path, bundleName string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read %s file: %w", bundleName, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("parse %s file: no certificates found", bundleName)
	}

	return pool, nil
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
		return nil, fmt.Errorf("parse backend CA file: no certificates found")
	}

	return rootCAs, nil
}
