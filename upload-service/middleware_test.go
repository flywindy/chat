package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgoidc "github.com/hmchangw/chat/pkg/oidc"
)

type fakeValidator struct {
	claims pkgoidc.Claims
	err    error
}

func (f *fakeValidator) Validate(_ context.Context, _ string) (pkgoidc.Claims, error) {
	return f.claims, f.err
}

func runAuth(t *testing.T, v TokenValidator, devMode bool, token string) (*httptest.ResponseRecorder, *AuthenticatedUser) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(authMiddleware(v, devMode))
	var captured *AuthenticatedUser
	r.GET("/x", func(c *gin.Context) {
		if u, ok := userFromContext(c); ok {
			captured = u
		}
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if token != "" {
		req.Header.Set("ssoToken", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, captured
}

func TestAuthMiddleware_MissingToken_401(t *testing.T) {
	w, _ := runAuth(t, &fakeValidator{}, false, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_InvalidToken_401(t *testing.T) {
	w, _ := runAuth(t, &fakeValidator{err: errors.New("bad")}, false, "tok")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_ExpiredToken_401(t *testing.T) {
	w, _ := runAuth(t, &fakeValidator{err: pkgoidc.ErrTokenExpired}, false, "tok")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_ValidToken_PopulatesUser(t *testing.T) {
	v := &fakeValidator{claims: pkgoidc.Claims{
		PreferredUsername: "alice",
		Email:             "alice@x.com",
		Description:       "E123, Alice, 陳大文",
	}}
	w, u := runAuth(t, v, false, "tok")
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, u)
	assert.Equal(t, "alice", u.Account)
	assert.Equal(t, "alice@x.com", u.Email)
	assert.Equal(t, "Alice", u.EngName)
	assert.Equal(t, "陳大文", u.ChineseName)
	assert.Equal(t, "Alice 陳大文", u.DisplayName())
}

func TestAuthMiddleware_DevMode_SynthesizesUser(t *testing.T) {
	w, u := runAuth(t, nil, true, "alice")
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, u)
	assert.Equal(t, "alice", u.Account)
	assert.Equal(t, "alice@dev.local", u.Email)
}

func TestRequestIDMiddleware_MintsAndEchoes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestIDMiddleware())
	r.GET("/x", func(c *gin.Context) {
		assert.NotEmpty(t, c.GetString("request_id"))
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
}

func TestAccessLogAndOtelMiddleware_PassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestIDMiddleware())
	r.Use(accessLogMiddleware())
	r.Use(otelMiddleware())
	called := false
	r.GET("/x", func(c *gin.Context) {
		called = true
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, w.Code)
}
