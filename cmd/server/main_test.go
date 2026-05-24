package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/apiservice-audit-proxy/pkg/telemetry"
)

func TestParseFlags_Validation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing backend url",
			args: []string{"--webhook-kubeconfig=/tmp/webhook.kubeconfig"},
			want: "--backend-url is required",
		},
		{
			name: "missing webhook kubeconfig",
			args: []string{"--backend-url=http://backend.local"},
			want: "--webhook-kubeconfig is required",
		},
		{
			name: "non positive capture size",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--max-audit-body-bytes=0",
			},
			want: "--max-audit-body-bytes must be greater than zero",
		},
		{
			name: "only tls cert",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--tls-cert-file=/tmp/tls.crt",
			},
			want: "--tls-cert-file and --tls-private-key-file must be provided together",
		},
		{
			name: "only tls key",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--tls-private-key-file=/tmp/tls.key",
			},
			want: "--tls-cert-file and --tls-private-key-file must be provided together",
		},
		{
			name: "only backend client cert",
			args: []string{
				"--backend-url=https://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--backend-insecure-skip-verify",
				"--backend-client-cert-file=/tmp/client.crt",
			},
			want: "--backend-client-cert-file and --backend-client-key-file must be provided together",
		},
		{
			name: "only backend client key",
			args: []string{
				"--backend-url=https://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--backend-insecure-skip-verify",
				"--backend-client-key-file=/tmp/client.key",
			},
			want: "--backend-client-cert-file and --backend-client-key-file must be provided together",
		},
		{
			name: "invalid backend identity mode",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--backend-identity-mode=bogus",
			},
			want: `--backend-identity-mode must be "requestheader" or "impersonation"`,
		},
		{
			name: "impersonation mode rejects backend client certificate",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--tls-cert-file=/tmp/tls.crt",
				"--tls-private-key-file=/tmp/tls.key",
				"--backend-identity-mode=impersonation",
				"--backend-client-cert-file=/tmp/client.crt",
				"--backend-client-key-file=/tmp/client.key",
			},
			want: "--backend-client-cert-file and --backend-client-key-file are not yet supported " +
				"with --backend-identity-mode=impersonation",
		},
		{
			name: "impersonation extra keys and forward all extras are mutually exclusive",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--tls-cert-file=/tmp/tls.crt",
				"--tls-private-key-file=/tmp/tls.key",
				"--backend-identity-mode=impersonation",
				"--backend-impersonation-extra-keys=scopes",
				"--backend-impersonation-forward-all-extras=true",
			},
			want: "--backend-impersonation-extra-keys and --backend-impersonation-forward-all-extras are mutually exclusive",
		},
		{
			name: "impersonation only flag rejected in requestheader mode",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--backend-impersonation-extra-keys=scopes",
			},
			want: "--backend-impersonation-extra-keys requires --backend-identity-mode=impersonation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseFlags(tc.args, io.Discard)
			require.Error(t, err)
			assert.EqualError(t, err, tc.want)
		})
	}
}

func TestParseFlags_ImpersonationMode_Accepted(t *testing.T) {
	t.Parallel()

	cfg, err := parseFlags([]string{
		"--backend-url=http://backend.local",
		"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
		"--tls-cert-file=/tmp/tls.crt",
		"--tls-private-key-file=/tmp/tls.key",
		"--backend-identity-mode=impersonation",
		"--backend-impersonation-token-file=/tmp/token",
		"--backend-impersonation-extra-keys=scopes,example.com/tenant",
		"--backend-impersonation-forward-uid=false",
	}, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, "impersonation", cfg.backendIdentityMode)
	assert.Equal(t, "/tmp/token", cfg.backendImpersonationTokenFile)
	assert.Equal(t, "scopes,example.com/tenant", cfg.backendImpersonationExtraKeys)
	assert.False(t, cfg.backendImpersonationForwardUID)
	assert.False(t, cfg.backendImpersonationForwardAllExtras)
}

func TestParseFlags_MetricsListenAddress(t *testing.T) {
	t.Parallel()

	cfg, err := parseFlags([]string{
		"--backend-url=http://backend.local",
		"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
		"--metrics-listen-address=:9090",
	}, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, ":9090", cfg.metricsListenAddress)
}

func TestWrapImpersonationTransport_MissingTokenFile(t *testing.T) {
	t.Parallel()

	_, err := wrapImpersonationTransport(http.DefaultTransport, filepath.Join(t.TempDir(), "missing-token"))
	require.Error(t, err)
}

func TestWrapImpersonationTransport_ReadsExistingTokenFile(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("a-token"), 0o600))

	roundTripper, err := wrapImpersonationTransport(http.DefaultTransport, tokenPath)
	require.NoError(t, err)
	assert.NotNil(t, roundTripper)
}

func TestSplitCommaList(t *testing.T) {
	t.Parallel()

	assert.Nil(t, splitCommaList(""))
	assert.Nil(t, splitCommaList("  "))
	assert.Equal(t, []string{"a", "b", "c"}, splitCommaList("a, b ,c"))
	assert.Equal(t, []string{"a", "b"}, splitCommaList("a,,b,"))
}

func TestBuildBackendTransport_Validation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		backendURL string
		cfg        config
		want       string
	}{
		{
			name:       "http backend rejects tls flags",
			backendURL: "http://backend.local",
			cfg: config{
				backendInsecureSkipVerify: true,
			},
			want: "backend TLS flags require an https --backend-url",
		},
		{
			name:       "unsupported backend scheme",
			backendURL: "ftp://backend.local",
			cfg:        config{},
			want:       "unsupported --backend-url scheme \"ftp\"",
		},
		{
			name:       "https backend requires explicit trust mode",
			backendURL: "https://backend.local",
			cfg:        config{},
			want:       "https --backend-url requires --backend-insecure-skip-verify or --backend-ca-file",
		},
		{
			name:       "https backend rejects conflicting trust modes",
			backendURL: "https://backend.local",
			cfg: config{
				backendInsecureSkipVerify: true,
				backendCAFile:             "/tmp/backend-ca.pem",
			},
			want: "--backend-insecure-skip-verify and --backend-ca-file are mutually exclusive",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backendURL, err := url.Parse(tc.backendURL)
			require.NoError(t, err)

			_, err = buildBackendTransport(backendURL, tc.cfg)
			require.Error(t, err)
			assert.EqualError(t, err, tc.want)
		})
	}
}

func TestBuildBackendTransport_HTTPSInsecureSkipVerify_Succeeds(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer tlsBackend.Close()

	backendURL, err := url.Parse(tlsBackend.URL)
	require.NoError(t, err)

	transport, err := buildBackendTransport(backendURL, config{
		backendInsecureSkipVerify: true,
	})
	require.NoError(t, err)
	require.NotNil(t, transport.TLSClientConfig)
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
	}

	resp, err := client.Get(tlsBackend.URL)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestBuildBackendTransport_BackendCAFile_Succeeds(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer tlsBackend.Close()

	backendURL, err := url.Parse(tlsBackend.URL)
	require.NoError(t, err)

	caFile := writeBackendCertFile(t, tlsBackend.TLS.Certificates[0].Certificate[0])

	transport, err := buildBackendTransport(backendURL, config{
		backendCAFile: caFile,
	})
	require.NoError(t, err)
	require.NotNil(t, transport.TLSClientConfig)
	assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
	}

	resp, err := client.Get(tlsBackend.URL)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestBuildBackendTransport_BackendServerName_IsApplied(t *testing.T) {
	t.Parallel()

	backendURL, err := url.Parse("https://backend.local")
	require.NoError(t, err)

	transport, err := buildBackendTransport(backendURL, config{
		backendInsecureSkipVerify: true,
		backendServerName:         "wardle-backend.internal",
	})
	require.NoError(t, err)

	require.NotNil(t, transport.TLSClientConfig)
	assert.Equal(t, "wardle-backend.internal", transport.TLSClientConfig.ServerName)
}

func TestBuildBackendTransport_BackendClientCertificate_IsLoaded(t *testing.T) {
	t.Parallel()

	backendURL, err := url.Parse("https://backend.local")
	require.NoError(t, err)

	certFile, keyFile := writeClientKeyPair(t)

	transport, err := buildBackendTransport(backendURL, config{
		backendInsecureSkipVerify: true,
		backendClientCertFile:     certFile,
		backendClientKeyFile:      keyFile,
	})
	require.NoError(t, err)

	require.NotNil(t, transport.TLSClientConfig)
	require.Len(t, transport.TLSClientConfig.Certificates, 1)
	assert.NotEmpty(t, transport.TLSClientConfig.Certificates[0].Certificate)
}

func TestBuildServingTLSConfig_RequestsClientCertWithoutStaticPool(t *testing.T) {
	t.Parallel()

	// Inbound trust is cluster-sourced: the TLS layer only requests a client
	// certificate, it does not verify it against a static pool. The
	// requestheader x509 verifier — backed by the dynamic cluster CA — is the
	// single inbound trust authority.
	tlsConfig := buildServingTLSConfig(config{
		tlsCertFile:       "/tmp/tls.crt",
		tlsPrivateKeyFile: "/tmp/tls.key",
	})
	require.NotNil(t, tlsConfig)
	assert.EqualValues(t, tls.VersionTLS12, tlsConfig.MinVersion)
	assert.Equal(t, tls.RequestClientCert, tlsConfig.ClientAuth)
	assert.Nil(t, tlsConfig.ClientCAs, "no static client CA pool with cluster-sourced trust")
}

func TestBuildServingTLSConfig_PlainHTTPWhenNoCert(t *testing.T) {
	t.Parallel()

	assert.Nil(t, buildServingTLSConfig(config{}))
}

func TestNewHTTPServer_UsesStreamingSafeTimeouts(t *testing.T) {
	t.Parallel()

	server, err := newHTTPServer(":0", http.HandlerFunc(handleHealth), nil)
	require.NoError(t, err)

	assert.Zero(t, server.WriteTimeout, "watch responses must not be killed by a fixed write deadline")
	assert.Zero(t, server.ReadTimeout, "use ReadHeaderTimeout so large or long-running requests are not body-deadlined")
	assert.Equal(t, 15*time.Second, server.ReadHeaderTimeout)
	assert.Equal(t, defaultIdleTimeout, server.IdleTimeout)
}

func TestNewMetricsServer_UsesPlainHTTPMetricsPort(t *testing.T) {
	t.Parallel()

	server := newMetricsServer(":0")
	require.NotNil(t, server)
	assert.Equal(t, ":0", server.Addr)
	assert.Equal(t, defaultReadHeaderTimeout, server.ReadHeaderTimeout)
	assert.Equal(t, defaultIdleTimeout, server.IdleTimeout)
	assert.NotNil(t, server.ConnState)

	assert.Nil(t, newMetricsServer("0"))
	assert.Nil(t, newMetricsServer(""))
}

func TestConnStateTracker_RecordsConnectionStateGauge(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	clientConn, serverConn := net.Pipe()
	defer func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}()

	observe := newConnStateTracker()
	observe(serverConn, http.StateNew)

	newCount, ok := telemetry.CollectInt64Sum(reader,
		"apiservice_audit_proxy_connections_active",
		map[string]string{"state": http.StateNew.String()})
	require.True(t, ok)
	assert.Equal(t, int64(1), newCount)

	observe(serverConn, http.StateActive)

	newCount, ok = telemetry.CollectInt64Sum(reader,
		"apiservice_audit_proxy_connections_active",
		map[string]string{"state": http.StateNew.String()})
	require.True(t, ok)
	assert.Equal(t, int64(0), newCount)

	activeCount, ok := telemetry.CollectInt64Sum(reader,
		"apiservice_audit_proxy_connections_active",
		map[string]string{"state": http.StateActive.String()})
	require.True(t, ok)
	assert.Equal(t, int64(1), activeCount)
}

func writeBackendCertFile(t *testing.T, certDER []byte) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "backend-ca.pem")

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	require.NotEmpty(t, pemBytes)
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))

	return path
}

func writeClientKeyPair(t *testing.T) (string, string) {
	t.Helper()

	_, certPEM, keyPEM := writeSignedClientCertificate(t, "audit-pass-through-proxy-ca", "audit-pass-through-proxy")

	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")

	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))

	return certPath, keyPath
}

func writeSignedClientCertificate(t *testing.T, caCommonName, clientCommonName string) ([]byte, []byte, []byte) {
	t.Helper()

	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: caCommonName,
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivateKey.PublicKey, caPrivateKey)
	require.NoError(t, err)

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName:   clientCommonName,
			Organization: []string{"system:masters"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &privateKey.PublicKey, caPrivateKey)
	require.NoError(t, err)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	require.NotEmpty(t, caPEM)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	require.NotEmpty(t, certPEM)

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	require.NotEmpty(t, keyPEM)

	return caPEM, certPEM, keyPEM
}
