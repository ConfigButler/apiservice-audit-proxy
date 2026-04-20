package identity

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
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
