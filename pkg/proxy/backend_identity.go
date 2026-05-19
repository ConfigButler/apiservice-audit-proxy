package proxy

import (
	"fmt"
	"net/http"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/transport"
)

// BackendIdentity decorates the outbound backend request with the identity the
// proxy presents to the real aggregated backend.
//
// Apply has no error return: header projection and stripping cannot fail, and
// the one fallible operation — obtaining the backend bearer token — is handled
// by the transport. An infallible Apply is what lets the same decorator run
// from the httputil.ReverseProxy Rewrite hook, which has no error channel.
//
// Implementations must mutate only the outbound request clone, never the
// inbound request that is later used to build the audit event.
type BackendIdentity interface {
	Apply(req *http.Request, user authnv1.UserInfo)
}

// RequestHeaderForwarder is the default BackendIdentity. It forwards the
// front-proxy's X-Remote-* identity headers to the backend unchanged, so the
// backend's requestheader authentication still sees the original delegated
// user.
//
// It is not a pure pass-through: it strips inbound Impersonate-* and
// Authorization headers. Those are never part of the requestheader-forwarding
// contract, so a client must not be able to smuggle them through the proxy to
// the backend.
type RequestHeaderForwarder struct{}

// Apply strips inbound Impersonate-* and Authorization headers while leaving
// the X-Remote-* requestheader identity in place for the backend hop.
func (RequestHeaderForwarder) Apply(req *http.Request, _ authnv1.UserInfo) {
	stripImpersonationHeaders(req.Header)
}

// ImpersonatorConfig configures an Impersonator.
type ImpersonatorConfig struct {
	// ExtraKeys is the allowlist of decoded extra keys projected into
	// Impersonate-Extra-* headers. Keys outside this list are dropped.
	ExtraKeys []string
	// ForwardAllExtras projects every inbound extra regardless of ExtraKeys.
	ForwardAllExtras bool
	// ForwardUID projects Impersonate-Uid when the identity has a non-empty UID.
	ForwardUID bool
}

// Impersonator projects the verified inbound identity into Kubernetes
// impersonation headers on the backend request. It strips any inbound
// X-Remote-*, Impersonate-*, and Authorization headers before setting its own
// controlled values, so the backend authenticates the request as an ordinary
// bearer-token request rather than as a trusted requestheader proxy request.
type Impersonator struct {
	extraKeyAllowlist map[string]struct{}
	forwardAllExtras  bool
	forwardUID        bool
}

// NewImpersonator builds an Impersonator from the given configuration.
func NewImpersonator(cfg ImpersonatorConfig) *Impersonator {
	allowlist := make(map[string]struct{}, len(cfg.ExtraKeys))
	for _, key := range cfg.ExtraKeys {
		allowlist[key] = struct{}{}
	}

	return &Impersonator{
		extraKeyAllowlist: allowlist,
		forwardAllExtras:  cfg.ForwardAllExtras,
		forwardUID:        cfg.ForwardUID,
	}
}

// Apply strips inbound identity headers and sets proxy-controlled
// impersonation headers derived from the verified user.
func (i *Impersonator) Apply(req *http.Request, user authnv1.UserInfo) {
	stripInboundIdentityHeaders(req.Header)

	req.Header.Set(transport.ImpersonateUserHeader, user.Username)
	for _, group := range user.Groups {
		req.Header.Add(transport.ImpersonateGroupHeader, group)
	}
	if i.forwardUID && user.UID != "" {
		req.Header.Set(transport.ImpersonateUIDHeader, user.UID)
	}

	for key, values := range user.Extra {
		if !i.forwardAllExtras {
			if _, ok := i.extraKeyAllowlist[key]; !ok {
				continue
			}
		}
		headerKey := transport.ImpersonateUserExtraHeaderPrefix + headerKeyEscape(key)
		for _, value := range values {
			req.Header.Add(headerKey, value)
		}
	}
}

// stripInboundIdentityHeaders removes every X-Remote-*, Impersonate-*, and
// Authorization header from the request. The Impersonator uses this to drop
// the whole inbound identity surface before setting its own controlled values.
func stripInboundIdentityHeaders(header http.Header) {
	deleteHeadersFunc(header, func(canonical string) bool {
		return canonical == "Authorization" ||
			strings.HasPrefix(canonical, "X-Remote-") ||
			strings.HasPrefix(canonical, "Impersonate-")
	})
}

// stripImpersonationHeaders removes every Impersonate-* and Authorization
// header while leaving X-Remote-* identity headers intact. The
// RequestHeaderForwarder uses this so a client cannot smuggle impersonation or
// bearer credentials through the requestheader-forwarding path.
func stripImpersonationHeaders(header http.Header) {
	deleteHeadersFunc(header, func(canonical string) bool {
		return canonical == "Authorization" ||
			strings.HasPrefix(canonical, "Impersonate-")
	})
}

// deleteHeadersFunc deletes every header whose canonicalized key satisfies
// match.
//
// http.Header.Del matches a single canonicalized key, so a fixed Del list
// cannot cover the open-ended X-Remote-Extra-* and Impersonate-Extra-* keys.
// Scanning every header key by predicate covers those prefixes instead.
func deleteHeadersFunc(header http.Header, match func(canonical string) bool) {
	for key := range header {
		if match(http.CanonicalHeaderKey(key)) {
			delete(header, key)
		}
	}
}

// headerKeyEscape percent-encodes an extra key the same way client-go's
// transport package does, so the backend apiserver decodes it identically.
// Bytes that are illegal in an HTTP header key are percent-encoded, and a
// literal '%' is encoded as well so url.PathUnescape on the backend succeeds.
func headerKeyEscape(key string) string {
	var buf strings.Builder
	for i := range len(key) {
		b := key[i]
		if shouldEscapeHeaderKeyByte(b) {
			fmt.Fprintf(&buf, "%%%02X", b)
			continue
		}
		buf.WriteByte(b)
	}

	return buf.String()
}

func shouldEscapeHeaderKeyByte(b byte) bool {
	return !legalHeaderKeyByte(b) || b == '%'
}

func legalHeaderKeyByte(b byte) bool {
	table := legalHeaderKeyBytes()
	return int(b) < len(table) && table[b]
}

// legalHeaderKeyBytes mirrors client-go transport's legalHeaderKeyBytes table,
// itself copied from net/http/lex.go's isTokenTable.
func legalHeaderKeyBytes() [127]bool {
	return [127]bool{
		'%':  true,
		'!':  true,
		'#':  true,
		'$':  true,
		'&':  true,
		'\'': true,
		'*':  true,
		'+':  true,
		'-':  true,
		'.':  true,
		'0':  true,
		'1':  true,
		'2':  true,
		'3':  true,
		'4':  true,
		'5':  true,
		'6':  true,
		'7':  true,
		'8':  true,
		'9':  true,
		'A':  true,
		'B':  true,
		'C':  true,
		'D':  true,
		'E':  true,
		'F':  true,
		'G':  true,
		'H':  true,
		'I':  true,
		'J':  true,
		'K':  true,
		'L':  true,
		'M':  true,
		'N':  true,
		'O':  true,
		'P':  true,
		'Q':  true,
		'R':  true,
		'S':  true,
		'T':  true,
		'U':  true,
		'W':  true,
		'V':  true,
		'X':  true,
		'Y':  true,
		'Z':  true,
		'^':  true,
		'_':  true,
		'`':  true,
		'a':  true,
		'b':  true,
		'c':  true,
		'd':  true,
		'e':  true,
		'f':  true,
		'g':  true,
		'h':  true,
		'i':  true,
		'j':  true,
		'k':  true,
		'l':  true,
		'm':  true,
		'n':  true,
		'o':  true,
		'p':  true,
		'q':  true,
		'r':  true,
		's':  true,
		't':  true,
		'u':  true,
		'v':  true,
		'w':  true,
		'x':  true,
		'y':  true,
		'z':  true,
		'|':  true,
		'~':  true,
	}
}
