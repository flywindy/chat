package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/errcode/errtest"
)

func setupRouter(t *testing.T, h *PortalHandler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerRoutes(r, h)
	return r
}

// getUserInfo issues GET /api/userInfo with account as a query parameter. An
// empty account is sent with no query parameter at all (the missing-param case).
func getUserInfo(t *testing.T, r *gin.Engine, account string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/userInfo"
	if account != "" {
		target += "?account=" + url.QueryEscape(account)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func getPath(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// cacheWith returns a directory cache populated with the given entries.
func cacheWith(emps ...employee) *directoryCache {
	c := newDirectoryCache()
	c.replace(emps)
	return c
}

// testSites is the per-site URL registry used by the handler tests, with a
// distinct domain per site to prove URLs are looked up, not templated.
var testSites = map[string]siteURL{
	"site-a":     {AuthServiceURL: "https://auth.site-a.example.com", BaseURL: "https://site-a.example.com"},
	"site-b":     {AuthServiceURL: "https://auth.site-b.example.com", BaseURL: "https://site-b.example.com"},
	"site-local": {AuthServiceURL: "https://auth.site-local.example.com", BaseURL: "http://localhost:3000"},
}

// testSettings is the settings payload used by the handler tests.
var testSettings = settingsResponse{
	APIVersion:  "v2",
	OTELBaseURL: "https://otel.example.com/v1",
}

// newTestHandler builds a PortalHandler with the test site registry and the
// local dev-fallback coordinates.
func newTestHandler(cache *directoryCache, devMode bool) *PortalHandler {
	return NewPortalHandler(cache, devMode, "site-local", "ws://localhost:9222", testSites, testSettings)
}

func TestHandleUserInfo_HappyPath(t *testing.T) {
	h := newTestHandler(cacheWith(aliceEmployee), false)
	w := getUserInfo(t, setupRouter(t, h), "alice")

	require.Equal(t, http.StatusOK, w.Code)
	var resp userInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, userInfoResponse{
		Account:        "alice",
		EmployeeID:     "E001",
		AuthServiceURL: "https://auth.site-a.example.com",
		BaseURL:        "https://site-a.example.com",
		NATSURL:        "wss://nats-3.site-a.example.com",
		SiteID:         "site-a",
	}, resp)
}

func TestHandleUserInfo_PerSiteURLs(t *testing.T) {
	t.Run("each site resolves its own auth and base URL from the registry", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee, bobEmployee), false)
		r := setupRouter(t, h)

		var alice userInfoResponse
		require.NoError(t, json.Unmarshal(getUserInfo(t, r, "alice").Body.Bytes(), &alice))
		assert.Equal(t, "https://auth.site-a.example.com", alice.AuthServiceURL)
		assert.Equal(t, "https://site-a.example.com", alice.BaseURL)

		var bob userInfoResponse
		require.NoError(t, json.Unmarshal(getUserInfo(t, r, "bob").Body.Bytes(), &bob))
		assert.Equal(t, "https://auth.site-b.example.com", bob.AuthServiceURL)
		assert.Equal(t, "https://site-b.example.com", bob.BaseURL)
	})

	t.Run("a site missing from the registry is a server error, not a client error", func(t *testing.T) {
		orphan := employee{Account: "carol", EmployeeID: "E003", SiteID: "site-unknown", NATSURL: "wss://nats.example.com"}
		h := newTestHandler(cacheWith(orphan), false)
		w := getUserInfo(t, setupRouter(t, h), "carol")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		errtest.AssertCode(t, w.Body.Bytes(), errcode.CodeInternal)
	})
}

func TestHandleUserInfo_InvalidAccountFormat(t *testing.T) {
	// Same token invariant auth-service enforces at minting — refusing here
	// keeps the portal from blessing an account the next step must reject.
	for _, tt := range []struct{ name, account string }{
		{"dotted account refused", "john.doe"},
		{"wildcard account refused", "mal*ory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(cacheWith(aliceEmployee), false)
			w := getUserInfo(t, setupRouter(t, h), tt.account)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			errtest.AssertCode(t, w.Body.Bytes(), errcode.CodeBadRequest)
		})
	}
}

func TestHandleUserInfo_MissingAccount(t *testing.T) {
	h := newTestHandler(cacheWith(aliceEmployee), false)
	w := getUserInfo(t, setupRouter(t, h), "")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	errtest.AssertCode(t, w.Body.Bytes(), errcode.CodeBadRequest)
	errtest.AssertReason(t, w.Body.Bytes(), errcode.AuthMissingFields)
}

func TestHandleUserInfo_AccountNotReady(t *testing.T) {
	t.Run("account absent from a populated cache", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee), false)
		w := getUserInfo(t, setupRouter(t, h), "mallory")

		assert.Equal(t, http.StatusForbidden, w.Code)
		errtest.AssertCode(t, w.Body.Bytes(), errcode.CodeForbidden)
		errtest.AssertReason(t, w.Body.Bytes(), errcode.PortalAccountNotReady)
	})

	t.Run("cache not yet loaded", func(t *testing.T) {
		h := newTestHandler(newDirectoryCache(), false)
		w := getUserInfo(t, setupRouter(t, h), "alice")

		assert.Equal(t, http.StatusForbidden, w.Code)
		errtest.AssertCode(t, w.Body.Bytes(), errcode.CodeForbidden)
		errtest.AssertReason(t, w.Body.Bytes(), errcode.PortalAccountNotReady)
	})
}

func TestHandleUserInfo_DevMode(t *testing.T) {
	t.Run("known account resolves normally", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee), true)
		w := getUserInfo(t, setupRouter(t, h), "alice")
		require.Equal(t, http.StatusOK, w.Code)
		var resp userInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "site-a", resp.SiteID)
	})

	t.Run("unknown account falls back to the dev site", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee), true)
		w := getUserInfo(t, setupRouter(t, h), "newdev")
		require.Equal(t, http.StatusOK, w.Code)
		var resp userInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, userInfoResponse{
			Account: "newdev", EmployeeID: "",
			AuthServiceURL: "https://auth.site-local.example.com", BaseURL: "http://localhost:3000",
			NATSURL: "ws://localhost:9222", SiteID: "site-local",
		}, resp)
	})

	t.Run("fallback works before the cache is loaded", func(t *testing.T) {
		h := newTestHandler(newDirectoryCache(), true)
		w := getUserInfo(t, setupRouter(t, h), "newdev")
		require.Equal(t, http.StatusOK, w.Code)
		var resp userInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "site-local", resp.SiteID)
	})

	t.Run("bad account format is refused even in dev mode", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee), true)
		w := getUserInfo(t, setupRouter(t, h), "mal*ory")
		assert.Equal(t, http.StatusBadRequest, w.Code)
		errtest.AssertCode(t, w.Body.Bytes(), errcode.CodeBadRequest)
	})

	t.Run("missing account is bad request", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee), true)
		w := getUserInfo(t, setupRouter(t, h), "")
		assert.Equal(t, http.StatusBadRequest, w.Code)
		errtest.AssertReason(t, w.Body.Bytes(), errcode.AuthMissingFields)
	})
}

func TestHandleHealth_AlwaysOK(t *testing.T) {
	// Liveness must not depend on directory data being loaded.
	h := newTestHandler(newDirectoryCache(), false)
	w := getPath(t, setupRouter(t, h), "/healthz")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "ok")
}

func TestHandleReady(t *testing.T) {
	tests := []struct {
		name     string
		cache    *directoryCache
		wantCode int
	}{
		{"cache never loaded", newDirectoryCache(), http.StatusServiceUnavailable},
		{"cache loaded but empty", cacheWith(), http.StatusServiceUnavailable},
		{"cache holds directory data", cacheWith(aliceEmployee), http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(tt.cache, false)
			w := getPath(t, setupRouter(t, h), "/readyz")
			assert.Equal(t, tt.wantCode, w.Code)
		})
	}
}

func TestHandleSettings(t *testing.T) {
	t.Run("returns the configured settings with exact field names", func(t *testing.T) {
		h := newTestHandler(cacheWith(aliceEmployee), false)
		w := getPath(t, setupRouter(t, h), "/api/settings")

		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t,
			`{"apiVersion":"v2","otelBaseUrl":"https://otel.example.com/v1"}`,
			w.Body.String())
		// Deployment config must not be cached by intermediaries.
		assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	})

	t.Run("serves before the directory cache is loaded", func(t *testing.T) {
		// Static config — must not depend on the directory refresh.
		h := newTestHandler(newDirectoryCache(), false)
		w := getPath(t, setupRouter(t, h), "/api/settings")

		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t,
			`{"apiVersion":"v2","otelBaseUrl":"https://otel.example.com/v1"}`,
			w.Body.String())
	})
}

func TestParseOTELBaseURL(t *testing.T) {
	for _, tt := range []struct{ name, raw, want string }{
		{"https URL passes through", "https://otel.example.com/v1", "https://otel.example.com/v1"},
		{"http URL passes through", "http://localhost:4318", "http://localhost:4318"},
		{"trailing slash trimmed", "https://otel.example.com/v1/", "https://otel.example.com/v1"},
		{"multiple trailing slashes trimmed", "https://otel.example.com/v1//", "https://otel.example.com/v1"},
		{"bare root slash trimmed", "https://otel.example.com/", "https://otel.example.com"},
		{"uppercase scheme normalized", "HTTPS://otel.example.com/v1", "https://otel.example.com/v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOTELBaseURL(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	for _, tt := range []struct{ name, raw string }{
		{"empty string", ""},
		{"garbage", "not a url"},
		{"relative URL", "/v1"},
		{"missing scheme", "otel.example.com/v1"},
		{"scheme without host", "https://"},
		{"non-http scheme", "ftp://otel.example.com"},
		{"control character", "https://otel.example.com/\n"},
		// The client appends /trace and /log — a base URL carrying a query,
		// fragment, or credentials would break or leak on that append.
		{"query string", "https://otel.example.com/v1?x=1"},
		{"force query", "https://otel.example.com/v1?"},
		{"fragment", "https://otel.example.com/v1#frag"},
		{"embedded credentials", "https://user:pass@otel.example.com/v1"},
	} {
		t.Run(tt.name+" is rejected", func(t *testing.T) {
			_, err := parseOTELBaseURL(tt.raw)
			assert.Error(t, err)
		})
	}
}

// setSettingsEnv sets a complete valid environment for config parsing;
// individual subtests override single vars to probe the notEmpty tags.
func setSettingsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PORTAL_SITE_URLS", `{"site-a":{"authServiceUrl":"https://auth.a.com","baseUrl":"https://a.com"}}`)
	t.Setenv("MONGO_URI", "mongodb://localhost:27017")
	t.Setenv("PORTAL_API_VERSION", "v2")
	t.Setenv("PORTAL_OTEL_BASE_URL", "https://otel.example.com/v1")
}

func TestConfig_SettingsEnvVars(t *testing.T) {
	t.Run("parses both settings fields", func(t *testing.T) {
		setSettingsEnv(t)
		cfg, err := env.ParseAs[config]()
		require.NoError(t, err)
		assert.Equal(t, "v2", cfg.APIVersion)
		assert.Equal(t, "https://otel.example.com/v1", cfg.OTELBaseURL)
	})

	for _, name := range []string{"PORTAL_API_VERSION", "PORTAL_OTEL_BASE_URL"} {
		t.Run("empty "+name+" is rejected", func(t *testing.T) {
			setSettingsEnv(t)
			t.Setenv(name, "")
			_, err := env.ParseAs[config]()
			assert.ErrorContains(t, err, name)
		})
	}
}

func TestParseOTELBaseURL_RejectionErrorRedactsPassword(t *testing.T) {
	// The rejection error is wrapped into the fatal startup log, so it must
	// not echo the credential the rule exists to reject.
	_, err := parseOTELBaseURL("https://user:hunter2@otel.example.com/v1")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestParseSiteURLs(t *testing.T) {
	t.Run("valid registry decodes per-site URLs", func(t *testing.T) {
		sites, err := parseSiteURLs(`{"site-a":{"authServiceUrl":"https://auth.a.com","baseUrl":"https://a.com"},"site-b":{"authServiceUrl":"https://auth.b.com","baseUrl":"https://b.com"}}`)
		require.NoError(t, err)
		assert.Equal(t, siteURL{AuthServiceURL: "https://auth.a.com", BaseURL: "https://a.com"}, sites["site-a"])
		assert.Equal(t, siteURL{AuthServiceURL: "https://auth.b.com", BaseURL: "https://b.com"}, sites["site-b"])
	})

	for _, tt := range []struct{ name, raw string }{
		{"not JSON", "site-a=https://auth.a.com"},
		{"empty object", "{}"},
		{"missing baseUrl", `{"site-a":{"authServiceUrl":"https://auth.a.com"}}`},
		{"missing authServiceUrl", `{"site-a":{"baseUrl":"https://a.com"}}`},
	} {
		t.Run(tt.name+" is rejected", func(t *testing.T) {
			_, err := parseSiteURLs(tt.raw)
			assert.Error(t, err)
		})
	}
}
