package pixapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies OAuth2 access tokens for API calls.
type TokenSource interface {
	// Token returns a valid access token.
	Token(ctx context.Context) (string, error)
}

// TokenInvalidator is implemented by token sources that cache tokens. The
// client calls Invalidate with the rejected token when the PSP answers 401,
// then retries the call once with a fresh token.
type TokenInvalidator interface {
	Invalidate(token string)
}

// StaticToken is a TokenSource that always returns the same token. It is
// meant for tests and short-lived scripts.
type StaticToken string

// Token implements TokenSource.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// Default timings of ClientCredentials.
const (
	DefaultEarlyRefresh = 60 * time.Second
	defaultTokenTimeout = 30 * time.Second
	// defaultTokenLifetime is assumed when the token endpoint omits
	// expires_in.
	defaultTokenLifetime = 5 * time.Minute
	refreshRetryDelay    = 5 * time.Second
)

// ClientCredentials is a TokenSource implementing the OAuth2
// client_credentials grant (RFC 6749, section 4.4), which is how the
// API Pix authenticates merchants. PSPs usually require mTLS on the token
// endpoint as well, so HTTPClient should carry the same client certificate
// as the API client; NewClient wires that up when HTTPClient is nil.
//
// Tokens are cached. A token is refreshed in the background once it is
// within EarlyRefresh of its expiry, while callers keep using the current
// one; only when no valid token exists do callers wait. Concurrent callers
// share a single request to the token endpoint (single-flight), and a
// caller giving up through its context does not cancel the shared fetch.
//
// The client secret is never logged and is redacted by String and by
// log/slog.
type ClientCredentials struct {
	// TokenURL is the PSP's token endpoint. It is not part of the API Pix
	// specification and differs between PSPs.
	TokenURL     string
	ClientID     string
	ClientSecret string
	// Scopes requested, e.g. "cob.write", "cob.read", "pix.read".
	Scopes []string
	// HTTPClient performs token requests. Nil means the API client's
	// HTTP client (when used through WithOAuth2) or http.DefaultClient.
	HTTPClient *http.Client
	// EarlyRefresh is how long before expiry the token is refreshed.
	// Zero means DefaultEarlyRefresh; it is capped at half the token
	// lifetime so short-lived tokens are not refreshed on every call.
	EarlyRefresh time.Duration
	// SecretInBody sends the credentials as form parameters instead of
	// HTTP Basic authentication, for PSPs that require it.
	SecretInBody bool

	now func() time.Time

	mu       sync.Mutex
	token    string
	expiry   time.Time // hard expiry
	refresh  time.Time // start of the early refresh window
	inflight *tokenCall
}

type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

// String implements fmt.Stringer without revealing the secret.
func (c *ClientCredentials) String() string {
	return fmt.Sprintf("ClientCredentials{TokenURL: %q, ClientID: %q, ClientSecret: <redacted>}", c.TokenURL, c.ClientID)
}

// LogValue implements slog.LogValuer without revealing the secret.
func (c *ClientCredentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token_url", c.TokenURL), slog.String("client_id", c.ClientID))
}

func (c *ClientCredentials) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Token implements TokenSource.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	now := c.clock()
	if c.token != "" && now.Before(c.expiry) {
		tok := c.token
		if !now.Before(c.refresh) && c.inflight == nil {
			c.startFetchLocked()
		}
		c.mu.Unlock()
		return tok, nil
	}
	call := c.inflight
	if call == nil {
		call = c.startFetchLocked()
	}
	c.mu.Unlock()

	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Invalidate implements TokenInvalidator. It drops the cached token only
// if it is still the rejected one, so a token fetched meanwhile survives.
func (c *ClientCredentials) Invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token != "" && c.token == token {
		c.token = ""
		c.expiry = time.Time{}
	}
}

// startFetchLocked starts a token request shared by all callers. c.mu must
// be held.
func (c *ClientCredentials) startFetchLocked() *tokenCall {
	call := &tokenCall{done: make(chan struct{})}
	c.inflight = call
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTokenTimeout)
		defer cancel()
		tok, lifetime, err := c.fetch(ctx)

		c.mu.Lock()
		if err == nil {
			now := c.clock()
			early := c.EarlyRefresh
			if early <= 0 {
				early = DefaultEarlyRefresh
			}
			if early > lifetime/2 {
				early = lifetime / 2
			}
			c.token = tok
			c.expiry = now.Add(lifetime)
			c.refresh = c.expiry.Add(-early)
		} else if c.token != "" {
			// A background refresh failed while the current token is
			// still valid: keep it and try again a little later rather
			// than on every call.
			c.refresh = c.clock().Add(refreshRetryDelay)
		}
		c.inflight = nil
		call.token, call.err = tok, err
		c.mu.Unlock()
		close(call.done)
	}()
	return call
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

func (c *ClientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(c.Scopes) > 0 {
		form.Set("scope", strings.Join(c.Scopes, " "))
	}
	if c.SecretInBody {
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("pixapi: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !c.SecretInBody {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("pixapi: token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("pixapi: token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		oe := &OAuthError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(body, oe)
		return "", 0, oe
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("pixapi: token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, errors.New("pixapi: token response without access_token")
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return "", 0, fmt.Errorf("pixapi: unsupported token type %q", tr.TokenType)
	}
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultTokenLifetime
	}
	return tr.AccessToken, lifetime, nil
}
