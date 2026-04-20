package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	auditproxy "github.com/ConfigButler/audit-pass-through-apiserver/pkg/proxy"
	"github.com/ConfigButler/audit-pass-through-apiserver/pkg/webhook"
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
	listenAddress     string
	backendURL        string
	webhookKubeconfig string
	webhookTimeout    time.Duration
	maxAuditBodyBytes int64
	captureTempDir    string
}

func main() {
	cfg := parseFlags()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	backendURL, err := url.Parse(cfg.backendURL)
	if err != nil {
		logger.Error("invalid backend URL", "error", err)
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
		Logger:            logger.With("component", "proxy"),
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

	logger.Info(
		"starting audit pass-through API server prototype",
		"listen_address", cfg.listenAddress,
		"backend_url", backendURL.String(),
		"webhook_kubeconfig", cfg.webhookKubeconfig,
		"max_audit_body_bytes", cfg.maxAuditBodyBytes,
		"capture_temp_dir", cfg.captureTempDir,
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

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}

	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&cfg.listenAddress, "listen-address", defaultListenAddress, "Address for the pass-through server.")
	fs.StringVar(&cfg.backendURL, "backend-url", "", "URL of the real aggregated backend.")
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
	fs.Parse(os.Args[1:])

	if cfg.backendURL == "" {
		fs.Usage()
		os.Exit(2)
	}
	if cfg.webhookKubeconfig == "" {
		fs.Usage()
		os.Exit(2)
	}
	if cfg.maxAuditBodyBytes <= 0 {
		fs.Usage()
		os.Exit(2)
	}

	return cfg
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
