package identity

import (
	"net/http"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
)

var standardRequestHeaderAuthenticator = mustAuthenticator()

func mustAuthenticator() authenticator.Request {
	authenticator, err := headerrequest.New(
		[]string{"X-Remote-User"},
		[]string{"X-Remote-Uid"},
		[]string{"X-Remote-Group"},
		[]string{"X-Remote-Extra-"},
	)
	if err != nil {
		panic(err)
	}

	return authenticator
}

// FromHeaders extracts the delegated user identity from the standard Kubernetes
// requestheader authentication surface.
func FromHeaders(headers http.Header) authnv1.UserInfo {
	request := &http.Request{Header: headers.Clone()}
	response, ok, err := standardRequestHeaderAuthenticator.AuthenticateRequest(request)
	if err != nil || !ok || response == nil || response.User == nil {
		return authnv1.UserInfo{}
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

	return userInfo
}
