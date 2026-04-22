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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			name: "client ca requires serving tls",
			args: []string{
				"--backend-url=http://backend.local",
				"--webhook-kubeconfig=/tmp/webhook.kubeconfig",
				"--client-ca-file=/tmp/client-ca.pem",
			},
			want: "--client-ca-file requires --tls-cert-file and --tls-private-key-file",
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

func TestBuildServingTLSConfig_ClientCA_IsApplied(t *testing.T) {
	t.Parallel()

	caCertPEM, _, _ := writeSignedClientCertificate(t, "front-proxy-ca", "kube-aggregator")
	caFile := filepath.Join(t.TempDir(), "client-ca.pem")
	require.NoError(t, os.WriteFile(caFile, caCertPEM, 0o600))

	tlsConfig, err := buildServingTLSConfig(config{
		tlsCertFile:       "/tmp/tls.crt",
		tlsPrivateKeyFile: "/tmp/tls.key",
		clientCAFile:      caFile,
	})
	require.NoError(t, err)
	require.NotNil(t, tlsConfig)
	assert.EqualValues(t, tls.VersionTLS12, tlsConfig.MinVersion)
	assert.Equal(t, tls.VerifyClientCertIfGiven, tlsConfig.ClientAuth)
	require.NotNil(t, tlsConfig.ClientCAs)
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
