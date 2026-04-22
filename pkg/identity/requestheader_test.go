package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFromHeaders_ExtractsDelegatedIdentity(t *testing.T) {
	userInfo := FromHeaders(http.Header{
		"X-Remote-User":                        {"alice"},
		"X-Remote-Uid":                         {"uid-alice"},
		"X-Remote-Group":                       {"devs", "admins"},
		"X-Remote-Extra-Example.com%2Ftenant":  {"team-a"},
		"X-Remote-Extra-Percent%20Encoded%20X": {"hello"},
	})

	assert.Equal(t, "alice", userInfo.Username)
	assert.Equal(t, "uid-alice", userInfo.UID)
	assert.Equal(t, []string{"devs", "admins"}, userInfo.Groups)
	assert.Equal(t, []string{"team-a"}, []string(userInfo.Extra["example.com/tenant"]))
	assert.Equal(t, []string{"hello"}, []string(userInfo.Extra["percent encoded x"]))
}

func TestExtractor_FromRequest_VerifiesClientCertificateWhenConfigured(t *testing.T) {
	t.Parallel()

	caFile, clientCertificate := writeClientCAFixture(t, "front-proxy-ca", "kube-aggregator")
	extractor, err := NewExtractor(caFile)
	require.NoError(t, err)

	request := &http.Request{
		Header: http.Header{
			"X-Remote-User":  {"alice"},
			"X-Remote-Group": {"devs"},
		},
		TLS: &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{clientCertificate},
		},
	}

	userInfo, ok, err := extractor.FromRequest(request)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "alice", userInfo.Username)
	assert.Equal(t, []string{"devs"}, userInfo.Groups)
}

func TestExtractor_FromRequest_RejectsMissingClientCertificateWhenConfigured(t *testing.T) {
	t.Parallel()

	caFile, _ := writeClientCAFixture(t, "front-proxy-ca", "kube-aggregator")
	extractor, err := NewExtractor(caFile)
	require.NoError(t, err)

	userInfo, ok, err := extractor.FromRequest(&http.Request{
		Header: http.Header{
			"X-Remote-User": {"alice"},
		},
	})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, userInfo.Username)
}

func writeClientCAFixture(t *testing.T, caCommonName, clientCommonName string) (string, *x509.Certificate) {
	t.Helper()

	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivateKey.PublicKey, caPrivateKey)
	require.NoError(t, err)

	clientPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	clientTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: clientCommonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	clientDER, err := x509.CreateCertificate(
		rand.Reader,
		clientTemplate,
		caCert,
		&clientPrivateKey.PublicKey,
		caPrivateKey,
	)
	require.NoError(t, err)

	caFile := filepath.Join(t.TempDir(), "client-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	require.NotEmpty(t, caPEM)
	require.NoError(t, os.WriteFile(caFile, caPEM, 0o600))

	clientCert, err := x509.ParseCertificate(clientDER)
	require.NoError(t, err)
	return caFile, clientCert
}
