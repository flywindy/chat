package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/errcode/errhttp"
	"github.com/hmchangw/chat/pkg/subject"
)

// siteURL holds a site's externally reachable HTTP URLs, looked up by siteId
// from the PORTAL_SITE_URLS registry. AuthServiceURL is where the client mints
// its JWT (POST /auth); BaseURL is the site's own origin, a distinct URL — not
// derived from AuthServiceURL. A single template can't express sites on
// different domains (siteA.xx.com vs siteB.yy.com), so each site is explicit.
type siteURL struct {
	AuthServiceURL string `json:"authServiceUrl"`
	BaseURL        string `json:"baseUrl"`
}

// parseSiteURLs decodes the PORTAL_SITE_URLS registry — a JSON object mapping
// siteId to that site's URLs — and requires every site to carry both URLs, so a
// misconfigured registry fails at startup rather than at a user's login.
func parseSiteURLs(raw string) (map[string]siteURL, error) {
	var sites map[string]siteURL
	if err := json.Unmarshal([]byte(raw), &sites); err != nil {
		return nil, fmt.Errorf("decode site URL registry: %w", err)
	}
	if len(sites) == 0 {
		return nil, fmt.Errorf("site URL registry is empty")
	}
	for id, s := range sites {
		if s.AuthServiceURL == "" || s.BaseURL == "" {
			return nil, fmt.Errorf("site %q: both authServiceUrl and baseUrl are required", id)
		}
	}
	return sites, nil
}

// settingsResponse is the deployment config served to the frontend: the
// backend API generation and the OTEL base URL (client appends /trace, /log).
type settingsResponse struct {
	APIVersion  string `json:"apiVersion"`
	OTELBaseURL string `json:"otelBaseUrl"`
}

// parseOTELBaseURL fails startup unless the value is an absolute http(s) URL
// safe for the client to append /trace and /log to: no credentials, query, or
// fragment, and no trailing slash (so the append can't produce "//trace").
func parseOTELBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse's error embeds the raw input; unwrap to the underlying
		// reason so a mistyped credential can't reach the startup log.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return "", fmt.Errorf("decode OTEL base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("OTEL base URL %q must be an absolute http(s) URL", u.Redacted())
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("OTEL base URL %q must not carry credentials, query, or fragment", u.Redacted())
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

type userInfoResponse struct {
	Account        string `json:"account"`
	EmployeeID     string `json:"employeeId"`
	AuthServiceURL string `json:"authServiceUrl"`
	BaseURL        string `json:"baseUrl"`
	NATSURL        string `json:"natsUrl"`
	SiteID         string `json:"siteId"`
}

// PortalHandler resolves a user's home-site coordinates from the in-memory
// directory cache. The cache holds only accounts present in both hr_employee
// and the users collection (intersected at load time), so a cache hit already
// means the account is a provisioned user. Discovery only: it serves non-secret
// directory data keyed by account and validates no token. The authoritative
// gate is auth-service, which validates the SSO token before minting a JWT.
type PortalHandler struct {
	cache              *directoryCache
	devMode            bool
	devFallbackSiteID  string
	devFallbackNatsURL string
	sites              map[string]siteURL
	settings           settingsResponse
}

// NewPortalHandler creates a PortalHandler. devMode synthesizes a dev-site
// entry for accounts absent from the directory so local logins need no seeding.
// sites is the siteId → URL registry used to resolve each account's home-site
// auth-service and base URLs.
func NewPortalHandler(cache *directoryCache, devMode bool, devFallbackSiteID, devFallbackNatsURL string, sites map[string]siteURL, settings settingsResponse) *PortalHandler {
	return &PortalHandler{
		cache:              cache,
		devMode:            devMode,
		devFallbackSiteID:  devFallbackSiteID,
		devFallbackNatsURL: devFallbackNatsURL,
		sites:              sites,
		settings:           settings,
	}
}

// HandleUserInfo resolves the home-site coordinates for the `account` query
// parameter. The frontend supplies the account directly — derived from the SSO
// token's preferred_username claim in production, or the dev login form in dev.
// No token is validated here; this endpoint is discovery only.
func (h *PortalHandler) HandleUserInfo(c *gin.Context) {
	ctx := errcode.WithLogValues(c.Request.Context(), "request_id", c.GetString("request_id"))

	account := c.Query("account")
	if account == "" {
		errhttp.Write(ctx, c, errcode.BadRequest("account is required",
			errcode.WithReason(errcode.AuthMissingFields)))
		return
	}
	h.resolve(ctx, c, account)
}

// resolve answers from the directory cache — a single in-memory lookup, no
// per-request datastore round trips. In devMode an account absent from the
// directory is synthesized onto the dev site.
func (h *PortalHandler) resolve(ctx context.Context, c *gin.Context, account string) {
	if !subject.IsValidAccountToken(account) {
		errhttp.Write(ctx, c, errcode.BadRequest("account must be a single NATS subject token (no '.', '*', '>' or whitespace)"))
		return
	}
	ctx = errcode.WithLogValues(ctx, "account", account)

	e, ok := h.cache.Get(account)
	if !ok {
		if !h.devMode {
			errhttp.Write(ctx, c, errcode.Forbidden("account not ready for chat",
				errcode.WithReason(errcode.PortalAccountNotReady)))
			return
		}
		e = employee{Account: account, SiteID: h.devFallbackSiteID, NATSURL: h.devFallbackNatsURL}
	}

	site, ok := h.sites[e.SiteID]
	if !ok {
		// A directory entry homed on a site missing from the registry is an ops
		// misconfiguration, not a client error — surface it as internal.
		errhttp.Write(ctx, c, fmt.Errorf("no URLs configured for siteId %q", e.SiteID))
		return
	}

	c.JSON(http.StatusOK, userInfoResponse{
		Account:        e.Account,
		EmployeeID:     e.EmployeeID,
		AuthServiceURL: site.AuthServiceURL,
		BaseURL:        site.BaseURL,
		NATSURL:        e.NATSURL,
		SiteID:         e.SiteID,
	})
}

// HandleSettings serves the startup-validated frontend deployment config.
func (h *PortalHandler) HandleSettings(c *gin.Context) {
	// Deployment config must stay fresh across apiVersion bumps — force
	// caches to revalidate on every fetch.
	c.Header("Cache-Control", "no-cache")
	c.JSON(http.StatusOK, h.settings)
}

// HandleHealth is the liveness probe: the process is up and serving HTTP.
func (h *PortalHandler) HandleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// HandleReady is the readiness probe: fails until the directory cache holds data.
func (h *PortalHandler) HandleReady(c *gin.Context) {
	if !h.cache.Ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
