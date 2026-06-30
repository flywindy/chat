package msgraph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetPresencesByUserId_ROPC(t *testing.T) {
	var grant, user string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		grant = r.Form.Get("grant_type")
		user = r.Form.Get("username")
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "ptok", ExpiresIn: 3600}) // #nosec G117 -- test mock OAuth token
	}))
	defer tokenSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer ptok", r.Header.Get("Authorization"))
		assert.Contains(t, r.URL.Path, "/communications/getPresencesByUserId")
		var body struct {
			IDs []string `json:"ids"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, []string{"id1", "id2"}, body.IDs)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []Presence{
				{ID: "id1", Availability: "Busy", Activity: "InACall"},
				{ID: "id2", Availability: "Available", Activity: "Available"},
			},
		})
	}))
	defer graphSrv.Close()

	pc := NewPresenceClient(
		Config{TenantID: "t", ClientID: "c", ClientSecret: "s"},
		ROPCCredentials{Username: "svc@corp.com", Password: "pw"},
		WithTokenURL(tokenSrv.URL), WithBaseURL(graphSrv.URL),
	)
	res, err := pc.GetPresencesByUserId(context.Background(), []string{"id1", "id2"})
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "InACall", res[0].Activity)
	assert.Equal(t, "password", grant)
	assert.Equal(t, "svc@corp.com", user)
}

func TestGetPresencesByUserId_Empty(t *testing.T) {
	pc := NewPresenceClient(Config{TenantID: "t"}, ROPCCredentials{})
	res, err := pc.GetPresencesByUserId(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, res)
}
