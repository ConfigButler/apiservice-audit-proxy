package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	authnv1 "k8s.io/api/authentication/v1"
)

func newInboundRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://backend.local/apis/wardle.example.com/v1alpha1/flunders", nil)
	req.Header.Set("Authorization", "Bearer user-supplied-token")
	req.Header.Set("X-Remote-User", "evil")
	req.Header.Set("X-Remote-Group", "evil-group")
	req.Header.Set("X-Remote-Uid", "evil-uid")
	req.Header.Set("X-Remote-Extra-Injected", "value")
	req.Header.Set("Impersonate-User", "evil")
	req.Header.Set("Impersonate-Group", "evil-group")
	req.Header.Set("Impersonate-Extra-Injected", "value")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func sampleUser() authnv1.UserInfo {
	return authnv1.UserInfo{
		Username: "alice",
		UID:      "uid-alice",
		Groups:   []string{"devs", "admins"},
		Extra: map[string]authnv1.ExtraValue{
			"scopes":     {"read", "write"},
			"secret-key": {"do-not-forward"},
		},
	}
}

func TestImpersonator_Apply_ProjectsVerifiedIdentity(t *testing.T) {
	t.Parallel()

	req := newInboundRequest()
	imp := NewImpersonator(ImpersonatorConfig{ExtraKeys: []string{"scopes"}, ForwardUID: true})
	imp.Apply(req, sampleUser())

	assert.Equal(t, "alice", req.Header.Get("Impersonate-User"))
	assert.Equal(t, []string{"devs", "admins"}, req.Header.Values("Impersonate-Group"))
	assert.Equal(t, "uid-alice", req.Header.Get("Impersonate-Uid"))
	assert.Equal(t, []string{"read", "write"}, req.Header.Values("Impersonate-Extra-Scopes"))
	// Content-Type is unrelated to identity and must survive.
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
}

func TestImpersonator_Apply_StripsInboundIdentityHeaders(t *testing.T) {
	t.Parallel()

	req := newInboundRequest()
	imp := NewImpersonator(ImpersonatorConfig{ForwardUID: true})
	imp.Apply(req, sampleUser())

	// Inbound Authorization is stripped so the bearer wrapper can apply the
	// proxy ServiceAccount token.
	assert.Empty(t, req.Header.Get("Authorization"))
	// Inbound X-Remote-* is stripped, including the open-ended extra key.
	assert.Empty(t, req.Header.Get("X-Remote-User"))
	assert.Empty(t, req.Header.Get("X-Remote-Group"))
	assert.Empty(t, req.Header.Get("X-Remote-Uid"))
	assert.Empty(t, req.Header.Values("X-Remote-Extra-Injected"))
	// Inbound Impersonate-Extra-* the caller supplied is stripped; only the
	// proxy-controlled projection remains.
	assert.Empty(t, req.Header.Values("Impersonate-Extra-Injected"))
	assert.Equal(t, "alice", req.Header.Get("Impersonate-User"))
}

func TestImpersonator_Apply_ExtraKeyAllowlist(t *testing.T) {
	t.Parallel()

	t.Run("allowlist drops keys outside it", func(t *testing.T) {
		t.Parallel()
		req := newInboundRequest()
		imp := NewImpersonator(ImpersonatorConfig{ExtraKeys: []string{"scopes"}, ForwardUID: true})
		imp.Apply(req, sampleUser())

		assert.Equal(t, []string{"read", "write"}, req.Header.Values("Impersonate-Extra-Scopes"))
		assert.Empty(t, req.Header.Values("Impersonate-Extra-Secret-Key"))
	})

	t.Run("empty allowlist forwards no extras", func(t *testing.T) {
		t.Parallel()
		req := newInboundRequest()
		imp := NewImpersonator(ImpersonatorConfig{ForwardUID: true})
		imp.Apply(req, sampleUser())

		assert.Empty(t, req.Header.Values("Impersonate-Extra-Scopes"))
		assert.Empty(t, req.Header.Values("Impersonate-Extra-Secret-Key"))
	})

	t.Run("forward-all-extras projects every inbound extra", func(t *testing.T) {
		t.Parallel()
		req := newInboundRequest()
		imp := NewImpersonator(ImpersonatorConfig{ForwardAllExtras: true, ForwardUID: true})
		imp.Apply(req, sampleUser())

		assert.Equal(t, []string{"read", "write"}, req.Header.Values("Impersonate-Extra-Scopes"))
		assert.Equal(t, []string{"do-not-forward"}, req.Header.Values("Impersonate-Extra-Secret-Key"))
	})
}

func TestImpersonator_Apply_ForwardUIDFalse(t *testing.T) {
	t.Parallel()

	req := newInboundRequest()
	imp := NewImpersonator(ImpersonatorConfig{ForwardUID: false})
	imp.Apply(req, sampleUser())

	assert.Empty(t, req.Header.Get("Impersonate-Uid"))
	assert.Equal(t, "alice", req.Header.Get("Impersonate-User"))
}

func TestRequestHeaderForwarder_Apply_IsNoOp(t *testing.T) {
	t.Parallel()

	req := newInboundRequest()
	RequestHeaderForwarder{}.Apply(req, sampleUser())

	// The forwarder must leave the inbound headers untouched.
	assert.Equal(t, "evil", req.Header.Get("X-Remote-User"))
	assert.Equal(t, "Bearer user-supplied-token", req.Header.Get("Authorization"))
	assert.Equal(t, "evil", req.Header.Get("Impersonate-User"))
}

func TestHeaderKeyEscape_MatchesKubernetes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		key  string
		want string
	}{
		{name: "plain key is unchanged", key: "scopes", want: "scopes"},
		{name: "slash is percent-encoded", key: "example.com/tenant", want: "example.com%2Ftenant"},
		{name: "mixed case is preserved", key: "Test.example.com/thing.thing", want: "Test.example.com%2Fthing.thing"},
		{name: "space is percent-encoded", key: "with space", want: "with%20space"},
		{name: "literal percent is encoded as %25", key: "a%20b", want: "a%2520b"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, headerKeyEscape(tc.key))
		})
	}
}
