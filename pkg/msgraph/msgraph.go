// Package msgraph is a minimal Microsoft Graph client for the chat Teams
// integration. It supports the client-credentials (app-only) OAuth2 flow and
// creating an onlineMeeting. Only the surface room-service needs is exposed,
// and it sits behind the Client interface so the meetings RPC can be unit
// tested against a mock without reaching Azure.
package msgraph

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client is the Graph surface room-service depends on. Only the meetings RPC
// touches Graph, so this is intentionally tiny. Mocked in tests.
type Client interface {
	// CreateOnlineMeeting creates (or returns the existing) onlineMeeting on
	// behalf of the configured organizer and returns its ID and join URL. It
	// uses Graph's idempotent createOrGet endpoint keyed on req.ExternalID, so
	// concurrent or repeated calls with the same ExternalID return the same
	// meeting — Graph itself is the idempotency source of truth.
	CreateOnlineMeeting(ctx context.Context, req CreateOnlineMeetingRequest) (*OnlineMeeting, error)
}

// CreateOnlineMeetingRequest carries the attributes used to create a meeting.
type CreateOnlineMeetingRequest struct {
	// ExternalID is the stable per-room idempotency key passed to Graph's
	// createOrGet endpoint. Graph guarantees exactly one meeting per
	// (organizer, externalId), so repeated/concurrent calls with the same
	// ExternalID return the same meeting instead of creating duplicates.
	// Required: createOrGet rejects an empty externalId.
	ExternalID string
	// Subject is the meeting title shown in Teams.
	Subject string
	// OrganizerEmail is the user the meeting is created for (the organizer).
	// When empty the application-context default mailbox is used.
	OrganizerEmail string
	// AttendeeEmails are the invited attendees (excluding the organizer).
	AttendeeEmails []string
}

// OnlineMeeting is the subset of the Graph onlineMeeting resource we return.
type OnlineMeeting struct {
	ID      string `json:"id"`
	JoinURL string `json:"joinWebUrl"`
}

// Config holds the Azure app-registration credentials and tenant.
type Config struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// TLSInsecureSkipVerify disables Graph TLS verification. Opt-in, dev/on-prem
	// only (e.g. a self-signed cert fronting Graph). Never enable in production.
	TLSInsecureSkipVerify bool
}

const (
	defaultGraphBaseURL = "https://graph.microsoft.com/v1.0"
	graphScope          = "https://graph.microsoft.com/.default"
	// tokenExpirySkew is subtracted from the token's reported lifetime so the
	// cached token is refreshed before the server-side expiry.
	tokenExpirySkew = 60 * time.Second
)

// graphClient is the live (*Client) implementation.
type graphClient struct {
	cfg        Config
	httpClient *http.Client
	baseURL    string
	tokenURL   string

	mu      sync.Mutex
	token   string
	tokenAt time.Time // when the cached token expires
}

// Option customizes the client (used in tests to point at an httptest server).
type Option func(*graphClient)

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(g *graphClient) { g.httpClient = c }
}

// WithBaseURL overrides the Graph API base URL (no trailing slash).
func WithBaseURL(u string) Option {
	return func(g *graphClient) { g.baseURL = strings.TrimRight(u, "/") }
}

// WithTokenURL overrides the OAuth2 token endpoint.
func WithTokenURL(u string) Option {
	return func(g *graphClient) { g.tokenURL = u }
}

// New constructs a live Graph client for the given config.
func New(cfg Config, opts ...Option) Client {
	hc := &http.Client{Timeout: 30 * time.Second}
	if cfg.TLSInsecureSkipVerify {
		// Clone the default transport so proxy (ProxyFromEnvironment) and dial
		// settings survive — an on-prem Graph behind a self-signed cert is the
		// scenario most likely to also sit behind a corporate proxy.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// #nosec G402 -- InsecureSkipVerify is opt-in via TLSInsecureSkipVerify config for dev/on-prem environments
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec
		hc.Transport = tr
	}
	g := &graphClient{
		cfg:        cfg,
		httpClient: hc,
		baseURL:    defaultGraphBaseURL,
		tokenURL: fmt.Sprintf(
			"https://login.microsoftonline.com/%s/oauth2/v2.0/token",
			url.PathEscape(cfg.TenantID),
		),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// accessToken returns a cached bearer token, fetching a fresh one via the
// client-credentials grant when the cache is empty or near expiry.
func (g *graphClient) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.token != "" && time.Now().Before(g.tokenAt) {
		return g.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", g.cfg.ClientID)
	form.Set("client_secret", g.cfg.ClientSecret)
	form.Set("scope", graphScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode token response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		// Never log the credentials; surface the OAuth error code/description only.
		return "", fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, tr.Error)
	}

	g.token = tr.AccessToken
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= tokenExpirySkew {
		lifetime = tokenExpirySkew
	}
	g.tokenAt = time.Now().Add(lifetime - tokenExpirySkew)
	return g.token, nil
}

// onlineMeetingPayload is the Graph createOrGet-onlineMeeting request body.
// externalId is required by createOrGet and is the per-room idempotency key.
type onlineMeetingPayload struct {
	ExternalID   string               `json:"externalId"`
	Subject      string               `json:"subject,omitempty"`
	Participants *meetingParticipants `json:"participants,omitempty"`
}

type meetingParticipants struct {
	Attendees []meetingAttendee `json:"attendees,omitempty"`
}

type meetingAttendee struct {
	Upn string `json:"upn"`
}

func (g *graphClient) CreateOnlineMeeting(ctx context.Context, req CreateOnlineMeetingRequest) (*OnlineMeeting, error) {
	if req.ExternalID == "" {
		return nil, fmt.Errorf("create onlineMeeting: externalId is required for createOrGet idempotency")
	}
	token, err := g.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire graph token: %w", err)
	}

	// createOrGet pushes idempotency to Graph: it returns the existing meeting
	// for an (organizer, externalId) pair if one exists, otherwise creates one.
	// App-only context requires targeting a specific organizer mailbox via the
	// /users/{id}/onlineMeetings/createOrGet path; delegated context uses /me.
	// We use the organizer-scoped path when an organizer email is supplied.
	var endpoint string
	if req.OrganizerEmail != "" {
		endpoint = fmt.Sprintf("%s/users/%s/onlineMeetings/createOrGet", g.baseURL, url.PathEscape(req.OrganizerEmail))
	} else {
		endpoint = g.baseURL + "/me/onlineMeetings/createOrGet"
	}

	payload := onlineMeetingPayload{ExternalID: req.ExternalID, Subject: req.Subject}
	if len(req.AttendeeEmails) > 0 {
		attendees := make([]meetingAttendee, 0, len(req.AttendeeEmails))
		for _, email := range req.AttendeeEmails {
			attendees = append(attendees, meetingAttendee{Upn: email})
		}
		payload.Participants = &meetingParticipants{Attendees: attendees}
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal onlineMeeting payload: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build onlineMeeting request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create onlineMeeting: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read onlineMeeting response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// Never wrap the raw response body into the error/cause — it can carry
		// upstream payload. Parse the Graph error envelope and surface only the
		// status + sanitized error code.
		var graphErr struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(respBody, &graphErr)
		if graphErr.Error.Code != "" {
			return nil, fmt.Errorf("create onlineMeeting: graph returned status %d (%s)", resp.StatusCode, graphErr.Error.Code)
		}
		return nil, fmt.Errorf("create onlineMeeting: graph returned status %d", resp.StatusCode)
	}

	var meeting OnlineMeeting
	if err := json.Unmarshal(respBody, &meeting); err != nil {
		return nil, fmt.Errorf("decode onlineMeeting response: %w", err)
	}
	if meeting.JoinURL == "" {
		return nil, fmt.Errorf("create onlineMeeting: graph response missing joinWebUrl")
	}
	return &meeting, nil
}
