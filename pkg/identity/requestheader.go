package identity

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	x509request "k8s.io/apiserver/pkg/authentication/request/x509"
)

// Extractor resolves delegated requestheader identity from an inbound request.
type Extractor struct {
	authenticator           authenticator.Request
	requiresVerifiedHeaders bool
}

// NewExtractor builds a requestheader extractor.
//
// When clientCAFile is set, the extractor only trusts delegated headers after
// the inbound client certificate has been verified against that CA bundle.
func NewExtractor(clientCAFile string) (*Extractor, error) {
	authenticator, err := newAuthenticator(clientCAFile)
	if err != nil {
		return nil, err
	}

	return &Extractor{
		authenticator:           authenticator,
		requiresVerifiedHeaders: clientCAFile != "",
	}, nil
}

func newAuthenticator(clientCAFile string) (authenticator.Request, error) {
	nameHeaders := headerrequest.StaticStringSlice([]string{"X-Remote-User"})
	uidHeaders := headerrequest.StaticStringSlice([]string{"X-Remote-Uid"})
	groupHeaders := headerrequest.StaticStringSlice([]string{"X-Remote-Group"})
	extraPrefixes := headerrequest.StaticStringSlice([]string{"X-Remote-Extra-"})

	if clientCAFile == "" {
		return headerrequest.New(
			nameHeaders.Value(),
			uidHeaders.Value(),
			groupHeaders.Value(),
			extraPrefixes.Value(),
		)
	}

	verifyOptionsFn, err := x509request.NewStaticVerifierFromFile(filepath.Clean(clientCAFile))
	if err != nil {
		return nil, fmt.Errorf("load requestheader client CA file: %w", err)
	}
	if verifyOptionsFn == nil {
		return nil, errors.New("requestheader client CA verifier is not configured")
	}

	return headerrequest.NewDynamicVerifyOptionsSecure(
		verifyOptionsFn,
		headerrequest.StaticStringSlice(nil),
		nameHeaders,
		uidHeaders,
		groupHeaders,
		extraPrefixes,
	), nil
}

// RequiresVerifiedHeaders reports whether the extractor expects a verified
// upstream client certificate before delegated headers are trusted.
func (e *Extractor) RequiresVerifiedHeaders() bool {
	return e != nil && e.requiresVerifiedHeaders
}

// FromRequest extracts the delegated user identity from the standard
// Kubernetes requestheader authentication surface.
func (e *Extractor) FromRequest(request *http.Request) (authnv1.UserInfo, bool, error) {
	if e == nil || e.authenticator == nil || request == nil {
		return authnv1.UserInfo{}, false, nil
	}

	authRequest := request.Clone(request.Context())
	authRequest.Header = request.Header.Clone()

	response, ok, err := e.authenticator.AuthenticateRequest(authRequest)
	if err != nil || !ok || response == nil || response.User == nil {
		return authnv1.UserInfo{}, ok, err
	}

	userInfo := authnv1.UserInfo{
		Username: response.User.GetName(),
		UID:      response.User.GetUID(),
		Groups:   append([]string(nil), response.User.GetGroups()...),
	}
	if extras := response.User.GetExtra(); len(extras) > 0 {
		userInfo.Extra = make(map[string]authnv1.ExtraValue, len(extras))
		for key, values := range extras {
			userInfo.Extra[key] = append(authnv1.ExtraValue(nil), values...)
		}
	}

	return userInfo, true, nil
}

// FromHeaders extracts the delegated user identity from headers only.
func FromHeaders(headers http.Header) authnv1.UserInfo {
	extractor, err := NewExtractor("")
	if err != nil {
		return authnv1.UserInfo{}
	}

	userInfo, _, err := extractor.FromRequest(&http.Request{Header: headers.Clone()})
	if err != nil {
		return authnv1.UserInfo{}
	}

	return userInfo
}
