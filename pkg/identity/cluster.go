package identity

import (
	"context"
	"errors"
	"fmt"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/kubernetes"
)

// AuthenticationConfigMapNamespace and AuthenticationConfigMapName identify the
// ConfigMap kube-apiserver maintains for aggregated API server requestheader
// trust. The proxy reads its inbound trust from this single object.
const (
	AuthenticationConfigMapNamespace = "kube-system"
	AuthenticationConfigMapName      = "extension-apiserver-authentication"

	requestHeaderClientCAFileKey    = "requestheader-client-ca-file"
	requestHeaderUsernameHeadersKey = "requestheader-username-headers"
	requestHeaderUIDHeadersKey      = "requestheader-uid-headers"
	requestHeaderGroupHeadersKey    = "requestheader-group-headers"
	requestHeaderExtraHeadersPrefix = "requestheader-extra-headers-prefix"
	requestHeaderAllowedNamesKey    = "requestheader-allowed-names"
)

// RequestHeaderTrustController sources the proxy's inbound requestheader trust —
// the front-proxy CA bundle, the accepted client common names, and the identity
// header names — live from the kube-system/extension-apiserver-authentication
// ConfigMap.
//
// It mirrors the upstream
// k8s.io/apiserver/pkg/server/options.DynamicRequestHeaderController, which is
// not constructible outside that package without pulling in the whole
// RecommendedOptions surface. It embeds the same two standard controllers a
// generic aggregated API server uses, so CA rotation and requestheader
// configuration changes are picked up live, with no restart and no Secret.
type RequestHeaderTrustController struct {
	ca      *dynamiccertificates.ConfigMapCAController
	headers *headerrequest.RequestHeaderAuthRequestController
}

// newRequestHeaderTrustController wires the two upstream controllers against the
// extension-apiserver-authentication ConfigMap.
func newRequestHeaderTrustController(client kubernetes.Interface) (*RequestHeaderTrustController, error) {
	caController, err := dynamiccertificates.NewDynamicCAFromConfigMapController(
		"client-ca",
		AuthenticationConfigMapNamespace,
		AuthenticationConfigMapName,
		requestHeaderClientCAFileKey,
		client,
	)
	if err != nil {
		return nil, fmt.Errorf("create requestheader CA controller: %w", err)
	}

	headersController := headerrequest.NewRequestHeaderAuthRequestController(
		AuthenticationConfigMapName,
		AuthenticationConfigMapNamespace,
		client,
		requestHeaderUsernameHeadersKey,
		requestHeaderUIDHeadersKey,
		requestHeaderGroupHeadersKey,
		requestHeaderExtraHeadersPrefix,
		requestHeaderAllowedNamesKey,
	)

	return &RequestHeaderTrustController{ca: caController, headers: headersController}, nil
}

// RunOnce performs a single synchronous trust load.
//
// It surfaces the failures the embedded controllers report directly: denied
// RBAC (Forbidden) and malformed requestheader header-name JSON. A missing
// ConfigMap, a missing or unparsable CA bundle, and an empty username-header
// list are not reported here — the upstream controllers treat those as
// non-fatal — so callers must also gate on Ready before serving.
func (c *RequestHeaderTrustController) RunOnce(ctx context.Context) error {
	if c == nil {
		return errors.New("requestheader trust controller is not initialized")
	}

	return utilerrors.NewAggregate([]error{
		c.ca.RunOnce(ctx),
		c.headers.RunOnce(ctx),
	})
}

// Run watches the ConfigMap for the lifetime of ctx, adopting CA rotation and
// requestheader configuration changes live. workers is accepted for parity with
// the upstream controllers; the CA controller starts a single worker regardless.
func (c *RequestHeaderTrustController) Run(ctx context.Context, workers int) {
	if c == nil {
		return
	}

	go c.ca.Run(ctx, workers)
	go c.headers.Run(ctx, workers)
	<-ctx.Done()
}

// Ready reports whether the controller currently holds a usable trust snapshot:
// a CA bundle that parsed into x509 verify options, and at least one username
// header to read identity from. It is false until the initial load succeeds and
// stays true afterwards under last-known-good trust even if later watches fail.
func (c *RequestHeaderTrustController) Ready() bool {
	if c == nil {
		return false
	}

	if _, ok := c.ca.VerifyOptions(); !ok {
		return false
	}

	return len(c.headers.UsernameHeaders()) > 0
}

// NewClusterExtractor builds an Extractor whose requestheader trust — CA,
// allowed names, and identity header names — is sourced live from the
// kube-system/extension-apiserver-authentication ConfigMap.
//
// The returned controller must complete a strict initial sync (RunOnce plus a
// Ready gate) before the server starts, then run in the background to pick up
// trust rotation. A cluster-sourced Extractor always verifies the inbound
// client certificate; there is no unverified path.
func NewClusterExtractor(client kubernetes.Interface) (*Extractor, *RequestHeaderTrustController, error) {
	if client == nil {
		return nil, nil, errors.New("kubernetes client is required for cluster-sourced requestheader trust")
	}

	controller, err := newRequestHeaderTrustController(client)
	if err != nil {
		return nil, nil, err
	}

	authRequest := headerrequest.NewDynamicVerifyOptionsSecure(
		controller.ca.VerifyOptions,
		headerrequest.StringSliceProviderFunc(controller.headers.AllowedClientNames),
		headerrequest.StringSliceProviderFunc(controller.headers.UsernameHeaders),
		headerrequest.StringSliceProviderFunc(controller.headers.UIDHeaders),
		headerrequest.StringSliceProviderFunc(controller.headers.GroupHeaders),
		headerrequest.StringSliceProviderFunc(controller.headers.ExtraHeaderPrefixes),
	)

	return &Extractor{
		authenticator:           authRequest,
		requiresVerifiedHeaders: true,
	}, controller, nil
}
