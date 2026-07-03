package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/errcode/errhttp"
	pkgoidc "github.com/hmchangw/chat/pkg/oidc"
	"github.com/hmchangw/chat/pkg/subject"
)

// TokenValidator validates an SSO token and returns OIDC claims.
type TokenValidator interface {
	Validate(ctx context.Context, rawToken string) (pkgoidc.Claims, error)
}

type authRequest struct {
	SSOToken      string `json:"ssoToken" binding:"required"`
	NATSPublicKey string `json:"natsPublicKey" binding:"required"`
}

type devAuthRequest struct {
	Account       string `json:"account" binding:"required"`
	NATSPublicKey string `json:"natsPublicKey" binding:"required"`
}

type authResponse struct {
	NATSJWT  string       `json:"natsJwt"`
	UserInfo userInfoResp `json:"user"`
}

type userInfoResp struct {
	Email       string `json:"email"`
	Account     string `json:"account"`
	EmployeeID  string `json:"employeeId"`
	EngName     string `json:"engName"`
	ChineseName string `json:"chineseName"`
	DeptName    string `json:"deptName"`
	DeptID      string `json:"deptId"`
}

// AuthHandler processes auth requests, validates SSO tokens via OIDC,
// and returns signed NATS user JWTs with scoped permissions.
type AuthHandler struct {
	validator     TokenValidator
	signingKey    nkeys.KeyPair
	accountPubKey string
	jwtExpiry     time.Duration
	jwtJitter     float64        // fraction of jwtExpiry; 0 = fixed lifetime
	randFloat     func() float64 // injectable [0,1) source; defaults to crypto rand
	devMode       bool
}

// Option configures optional AuthHandler behavior.
type Option func(*AuthHandler)

// WithJitter sets the JWT-lifetime jitter fraction (clamped to [0, 0.9]) so a
// fleet of sessions minted together does not expire in lockstep.
func WithJitter(frac float64) Option {
	return func(h *AuthHandler) {
		if frac < 0 {
			frac = 0
		}
		if frac > 0.9 {
			frac = 0.9
		}
		h.jwtJitter = frac
	}
}

// WithRandFloat overrides the randomness source (test seam).
func WithRandFloat(fn func() float64) Option {
	return func(h *AuthHandler) { h.randFloat = fn }
}

// NewAuthHandler creates an AuthHandler with the given token validator, NATS
// account scoped signing key, the account public key that key belongs to (used
// for the JWT's issuer_account claim so the NATS resolver can look up the
// account), and JWT expiry duration.
func NewAuthHandler(validator TokenValidator, signingKey nkeys.KeyPair, accountPubKey string, jwtExpiry time.Duration, devMode bool, opts ...Option) *AuthHandler {
	h := &AuthHandler{
		validator:     validator,
		signingKey:    signingKey,
		accountPubKey: accountPubKey,
		jwtExpiry:     jwtExpiry,
		randFloat:     cryptoRandFloat,
		devMode:       devMode,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// cryptoRandFloat returns a uniform float in [0,1) from crypto/rand. On the
// (practically impossible) read error it returns 0.5 — the no-skew midpoint.
func cryptoRandFloat() float64 {
	const denom = 1 << 53
	n, err := rand.Int(rand.Reader, big.NewInt(denom))
	if err != nil {
		slog.Error("crypto/rand read failed, using no-skew midpoint for JWT jitter", "error", err)
		return 0.5
	}
	return float64(n.Int64()) / float64(denom)
}

// HandleAuth validates the SSO token, resolves permissions based on
// the user account, and returns a signed NATS JWT.
func (h *AuthHandler) HandleAuth(c *gin.Context) {
	if h.devMode {
		h.handleDevAuth(c)
		return
	}

	ctx := errcode.WithLogValues(c.Request.Context(), "request_id", c.GetString("request_id"))

	var req authRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errhttp.Write(ctx, c, errcode.BadRequest("ssoToken and natsPublicKey are required",
			errcode.WithReason(errcode.AuthMissingFields)))
		return
	}

	if !nkeys.IsValidPublicUserKey(req.NATSPublicKey) {
		errhttp.Write(ctx, c, errcode.BadRequest("invalid natsPublicKey format",
			errcode.WithReason(errcode.AuthInvalidNKey)))
		return
	}

	claims, err := h.validator.Validate(ctx, req.SSOToken)
	if err != nil {
		if errors.Is(err, pkgoidc.ErrTokenExpired) {
			errhttp.Write(ctx, c, errcode.Unauthenticated("SSO token has expired, please re-login",
				errcode.WithReason(errcode.AuthTokenExpired)))
			return
		}
		// Non-expiry failures surface as "invalid SSO token"; attach the raw
		// cause so the server log carries the actual reason.
		errhttp.Write(ctx, c, errcode.Unauthenticated("invalid SSO token",
			errcode.WithReason(errcode.AuthInvalidToken),
			errcode.WithCause(err)))
		return
	}

	account := claims.Account()
	if account == "" {
		// Blank account would mint a JWT with chat.user..> permissions — refuse.
		errhttp.Write(ctx, c, errcode.Unauthenticated("token missing account claim",
			errcode.WithReason(errcode.AuthInvalidToken)))
		return
	}
	if !subject.IsValidAccountToken(account) {
		errhttp.Write(ctx, c, errcode.BadRequest("account must be a single NATS subject token (no '.', '*', '>' or whitespace)"))
		return
	}
	ctx = errcode.WithLogValues(ctx, "account", account)

	natsJWT, err := h.signNATSJWT(req.NATSPublicKey, account)
	if err != nil {
		errhttp.Write(ctx, c, fmt.Errorf("generating NATS token: %w", err))
		return
	}

	slog.Debug("auth success", "account", account, "subject", claims.Subject)

	// Parse description field: "employeeId, engName, chineseName"
	employeeID, engName, chineseName := parseDescription(claims.Description)

	c.JSON(http.StatusOK, authResponse{
		NATSJWT: natsJWT,
		UserInfo: userInfoResp{
			Email:       claims.Email,
			Account:     account,
			EmployeeID:  employeeID,
			EngName:     engName,
			ChineseName: chineseName,
			DeptName:    claims.DeptName,
			DeptID:      claims.DeptID,
		},
	})
}

// handleDevAuth handles auth in dev mode: accepts account name directly
// without OIDC validation, for use during local development only.
func (h *AuthHandler) handleDevAuth(c *gin.Context) {
	ctx := errcode.WithLogValues(c.Request.Context(), "request_id", c.GetString("request_id"))

	var req devAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errhttp.Write(ctx, c, errcode.BadRequest("account and natsPublicKey are required",
			errcode.WithReason(errcode.AuthMissingFields)))
		return
	}

	if !nkeys.IsValidPublicUserKey(req.NATSPublicKey) {
		errhttp.Write(ctx, c, errcode.BadRequest("invalid natsPublicKey format",
			errcode.WithReason(errcode.AuthInvalidNKey)))
		return
	}

	if !subject.IsValidAccountToken(req.Account) {
		errhttp.Write(ctx, c, errcode.BadRequest("account must be a single NATS subject token (no '.', '*', '>' or whitespace)"))
		return
	}
	ctx = errcode.WithLogValues(ctx, "account", req.Account)

	natsJWT, err := h.signNATSJWT(req.NATSPublicKey, req.Account)
	if err != nil {
		errhttp.Write(ctx, c, fmt.Errorf("generating NATS token: %w", err))
		return
	}

	slog.Debug("dev auth success", "account", req.Account)

	c.JSON(http.StatusOK, authResponse{
		NATSJWT: natsJWT,
		UserInfo: userInfoResp{
			Email:   req.Account + "@dev.local",
			Account: req.Account,
			EngName: req.Account,
		},
	})
}

// signNATSJWT signs a scoped NATS user JWT. Permissions and limits come
// from the account's scoped signing key template; the account tag drives
// per-user subject substitution ({{tag(account)}}). IssuerAccount tells
// the NATS resolver which account this signing key belongs to, since the
// JWT's iss is the signing-key pubkey, not the account root.
//
// Effective grants declared on the scope template (kept in sync with
// docker-local/setup.sh; the prod template is owned by the platform team,
// so a change there must be mirrored here and in docs/client-api.md §2.1):
//
//	Pub allow:
//	  chat.user.{account}.>
//	  _INBOX.>
//	  chat.user.presence.*.query.batch
//	  (+ allow-pub-response, for NATS request/reply)
//	Sub allow:
//	  chat.user.{account}.>
//	  chat.room.>
//	  _INBOX.>
//	  chat.user.presence.state.*
func (h *AuthHandler) signNATSJWT(userPubKey, account string) (string, error) {
	uc := jwt.NewUserClaims(userPubKey)
	uc.IssuerAccount = h.accountPubKey
	uc.Expires = h.jwtExpiryAt().Unix()
	uc.Tags.Add("account:" + account)
	uc.SetScoped(true)
	return uc.Encode(h.signingKey)
}

// jwtExpiryAt returns the absolute expiry, applying ±jwtJitter around the base
// lifetime: factor = 1 + jitter*(2r-1), r in [0,1).
func (h *AuthHandler) jwtExpiryAt() time.Time {
	factor := 1 + h.jwtJitter*(2*h.randFloat()-1)
	return time.Now().Add(time.Duration(float64(h.jwtExpiry) * factor))
}

// parseDescription splits the description field "employeeId, engName, chineseName"
// into its three components.
func parseDescription(desc string) (employeeID, engName, chineseName string) {
	parts := strings.SplitN(desc, ",", 3)
	if len(parts) >= 1 {
		employeeID = strings.TrimSpace(parts[0])
	}
	if len(parts) >= 2 {
		engName = strings.TrimSpace(parts[1])
	}
	if len(parts) >= 3 {
		chineseName = strings.TrimSpace(parts[2])
	}
	return
}

func (h *AuthHandler) HandleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
