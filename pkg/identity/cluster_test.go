package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewClusterExtractor_VerifiesCertificateAgainstConfigMapCA(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "front-proxy-ca")
	trustedCert := ca.issueClient(t)
	foreignCert := newTestCA(t, "rogue-ca").issueClient(t)

	client := fake.NewSimpleClientset(authConfigMap(ca.pem))
	extractor, controller := startClusterExtractor(t, client)

	userInfo, ok, err := verifyClientCertificate(extractor, trustedCert)
	require.NoError(t, err)
	assert.True(t, ok, "certificate under the ConfigMap CA must verify")
	assert.Equal(t, "alice", userInfo.Username)
	assert.True(t, extractor.RequiresVerifiedHeaders())

	_, ok, _ = verifyClientCertificate(extractor, foreignCert)
	assert.False(t, ok, "certificate under a foreign CA must be rejected")

	assert.True(t, controller.Ready())
}

func TestNewClusterExtractor_ObservesCARotation(t *testing.T) {
	t.Parallel()

	caA := newTestCA(t, "front-proxy-ca")
	caB := newTestCA(t, "front-proxy-ca-rotated")
	certA := caA.issueClient(t)
	certB := caB.issueClient(t)

	client := fake.NewSimpleClientset(authConfigMap(caA.pem))
	extractor, _ := startClusterExtractor(t, client)

	_, ok, _ := verifyClientCertificate(extractor, certA)
	require.True(t, ok)

	updateAuthConfigMap(t, client, authConfigMap(caB.pem))

	// The same Extractor instance must adopt the rotated CA via the informer,
	// with no rebuild: certB starts verifying and certA stops.
	require.Eventually(t, func() bool {
		_, ok, _ := verifyClientCertificate(extractor, certB)
		return ok
	}, 5*time.Second, 20*time.Millisecond, "rotated CA was never adopted")

	_, ok, _ = verifyClientCertificate(extractor, certA)
	assert.False(t, ok, "certificate under the superseded CA must be rejected")
}

func TestRequestHeaderTrustController_NotReadyWhenConfigMapAbsent(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	_, controller, err := NewClusterExtractor(client)
	require.NoError(t, err)

	runController(t, controller)
	require.NoError(t, controller.RunOnce(context.Background()), "a missing ConfigMap is not a RunOnce error")

	assertNeverReady(t, controller)
}

func TestRequestHeaderTrustController_NotReadyWhenCABundleMissing(t *testing.T) {
	t.Parallel()

	cm := authConfigMap(nil)
	delete(cm.Data, requestHeaderClientCAFileKey)

	client := fake.NewSimpleClientset(cm)
	_, controller, err := NewClusterExtractor(client)
	require.NoError(t, err)

	runController(t, controller)
	_ = controller.RunOnce(context.Background())

	assertNeverReady(t, controller)
}

func TestRequestHeaderTrustController_NotReadyWhenCABundleInvalid(t *testing.T) {
	t.Parallel()

	cm := authConfigMap([]byte("not a pem certificate"))

	client := fake.NewSimpleClientset(cm)
	_, controller, err := NewClusterExtractor(client)
	require.NoError(t, err)

	runController(t, controller)
	_ = controller.RunOnce(context.Background())

	assertNeverReady(t, controller)
}

func TestRequestHeaderTrustController_RunOnceFailsOnMalformedHeaderJSON(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "front-proxy-ca")
	cm := authConfigMap(ca.pem)
	cm.Data[requestHeaderUsernameHeadersKey] = "not-json"

	client := fake.NewSimpleClientset(cm)
	_, controller, err := NewClusterExtractor(client)
	require.NoError(t, err)

	require.Error(t, controller.RunOnce(context.Background()),
		"malformed requestheader header JSON must fail RunOnce")
}

func TestRequestHeaderTrustController_NotReadyWhenUsernameHeadersEmpty(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "front-proxy-ca")
	cm := authConfigMap(ca.pem)
	cm.Data[requestHeaderUsernameHeadersKey] = `[]`

	client := fake.NewSimpleClientset(cm)
	_, controller, err := NewClusterExtractor(client)
	require.NoError(t, err)

	runController(t, controller)
	require.NoError(t, controller.RunOnce(context.Background()))

	assertNeverReady(t, controller)
}

func TestRequestHeaderTrustController_RetainsLastKnownGoodAfterBadUpdate(t *testing.T) {
	t.Parallel()

	caA := newTestCA(t, "front-proxy-ca")
	caB := newTestCA(t, "front-proxy-ca-recovered")
	certA := caA.issueClient(t)
	certB := caB.issueClient(t)

	client := fake.NewSimpleClientset(authConfigMap(caA.pem))
	extractor, controller := startClusterExtractor(t, client)

	_, ok, _ := verifyClientCertificate(extractor, certA)
	require.True(t, ok)

	// A later malformed CA bundle must not erase the working trust snapshot.
	updateAuthConfigMap(t, client, authConfigMap([]byte("garbage")))
	require.Never(t, func() bool {
		_, ok, _ := verifyClientCertificate(extractor, certA)
		return !ok || !controller.Ready()
	}, 500*time.Millisecond, 50*time.Millisecond, "last-known-good trust was dropped on a bad update")

	// A later good CA bundle is still adopted.
	updateAuthConfigMap(t, client, authConfigMap(caB.pem))
	require.Eventually(t, func() bool {
		_, ok, _ := verifyClientCertificate(extractor, certB)
		return ok
	}, 5*time.Second, 20*time.Millisecond, "recovered CA was never adopted")
}

func TestNewClusterExtractor_NilClient(t *testing.T) {
	t.Parallel()

	_, _, err := NewClusterExtractor(nil)
	require.Error(t, err)
}

// startClusterExtractor builds a cluster Extractor, starts its controller, and
// blocks until it reports a usable trust snapshot.
func startClusterExtractor(t *testing.T, client *fake.Clientset) (*Extractor, *RequestHeaderTrustController) {
	t.Helper()

	extractor, controller, err := NewClusterExtractor(client)
	require.NoError(t, err)

	runController(t, controller)
	require.NoError(t, controller.RunOnce(context.Background()))
	require.Eventually(t, controller.Ready, 5*time.Second, 20*time.Millisecond,
		"trust controller never became ready")

	return extractor, controller
}

// runController starts the controller's watch loop for the test's lifetime.
func runController(t *testing.T, controller *RequestHeaderTrustController) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go controller.Run(ctx, 1)
}

// assertNeverReady fails if the controller becomes ready within a short window.
func assertNeverReady(t *testing.T, controller *RequestHeaderTrustController) {
	t.Helper()

	require.Never(t, controller.Ready, 750*time.Millisecond, 50*time.Millisecond,
		"controller must not report ready without usable trust")
}

func verifyClientCertificate(extractor *Extractor, cert *x509.Certificate) (authnv1.UserInfo, bool, error) {
	return extractor.FromRequest(&http.Request{
		Header: http.Header{"X-Remote-User": {"alice"}, "X-Remote-Group": {"devs"}},
		TLS:    &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}},
	})
}

func authConfigMap(caPEM []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: AuthenticationConfigMapNamespace,
			Name:      AuthenticationConfigMapName,
		},
		Data: map[string]string{
			requestHeaderClientCAFileKey:    string(caPEM),
			requestHeaderUsernameHeadersKey: `["X-Remote-User"]`,
			requestHeaderUIDHeadersKey:      `["X-Remote-Uid"]`,
			requestHeaderGroupHeadersKey:    `["X-Remote-Group"]`,
			requestHeaderExtraHeadersPrefix: `["X-Remote-Extra-"]`,
			requestHeaderAllowedNamesKey:    `[]`,
		},
	}
}

func updateAuthConfigMap(t *testing.T, client *fake.Clientset, cm *corev1.ConfigMap) {
	t.Helper()

	_, err := client.CoreV1().ConfigMaps(AuthenticationConfigMapNamespace).
		Update(context.Background(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
}

type testCA struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, commonName string) testCA {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

func (ca testCA) issueClient(t *testing.T) *x509.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "kube-aggregator"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return cert
}
