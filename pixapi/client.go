package pixapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Version is the SDK version sent in the User-Agent header.
const Version = "0.1.0"

// DefaultRequestIDHeader is the header carrying the per-call request ID.
// The API Pix does not standardize one; set another with
// WithRequestIDHeader if the PSP documents its own.
const DefaultRequestIDHeader = "X-Request-Id"

// RetryPolicy controls retries of idempotent calls.
//
// Only GET, HEAD, PUT and DELETE are retried: they are idempotent by HTTP
// semantics, and the API Pix designs its PUT endpoints around a
// client-chosen identifier (txid, devolução id) precisely so that they can
// be repeated. POST and PATCH are never retried. Note that a PSP may answer
// a repeated PUT /cob/{txid} whose first attempt did succeed with an
// "operação inválida" problem; callers that need exactly-once semantics
// should follow up with GetCob.
//
// Retries happen on transport errors and on 429, 502, 503 and 504, with
// exponential backoff and full jitter, honoring Retry-After when present.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first.
	// Values below 1 mean 1.
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy is used unless WithRetry is given.
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second}

// Client calls the API Pix of a PSP. It is safe for concurrent use.
type Client struct {
	base        *url.URL
	hc          *http.Client
	tlsConfig   *tls.Config
	tokens      TokenSource
	oauth       *ClientCredentials
	retry       RetryPolicy
	timeout     time.Duration
	reqIDHeader string
	userAgent   string
	logger      *slog.Logger
	sleep       func(ctx context.Context, d time.Duration) error
}

// Option configures a Client.
type Option func(*Client) error

// WithHTTPClient sets the HTTP client. Its transport is used as is, so
// WithTLSConfig has no effect when both are given.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return errors.New("pixapi: nil http client")
		}
		c.hc = hc
		return nil
	}
}

// WithTLSConfig sets the TLS configuration used to reach the PSP, which is
// where the mTLS client certificate goes (see LoadClientTLS). The config is
// cloned; MinVersion is raised to TLS 1.2 if lower.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(c *Client) error {
		if cfg == nil {
			return errors.New("pixapi: nil tls config")
		}
		c.tlsConfig = cfg.Clone()
		if c.tlsConfig.MinVersion < tls.VersionTLS12 {
			c.tlsConfig.MinVersion = tls.VersionTLS12
		}
		return nil
	}
}

// WithTokenSource sets the source of OAuth2 access tokens.
func WithTokenSource(ts TokenSource) Option {
	return func(c *Client) error {
		c.tokens = ts
		return nil
	}
}

// WithOAuth2 authenticates with the client_credentials grant. The token
// endpoint is called through the client's own HTTP client, and thus with
// the same mTLS certificate.
func WithOAuth2(tokenURL, clientID, clientSecret string, scopes ...string) Option {
	return func(c *Client) error {
		if tokenURL == "" || clientID == "" {
			return errors.New("pixapi: token URL and client ID are required")
		}
		c.oauth = &ClientCredentials{TokenURL: tokenURL, ClientID: clientID, ClientSecret: clientSecret, Scopes: scopes}
		c.tokens = c.oauth
		return nil
	}
}

// WithRetry sets the retry policy for idempotent calls.
func WithRetry(p RetryPolicy) Option {
	return func(c *Client) error {
		if p.MaxAttempts < 1 {
			p.MaxAttempts = 1
		}
		c.retry = p
		return nil
	}
}

// WithTimeout bounds each attempt (not the whole call, which the caller
// bounds with its context). Zero disables the per-attempt timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		c.timeout = d
		return nil
	}
}

// WithRequestIDHeader sets the name of the request ID header; empty
// disables it.
func WithRequestIDHeader(name string) Option {
	return func(c *Client) error {
		c.reqIDHeader = name
		return nil
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) error {
		c.userAgent = ua
		return nil
	}
}

// WithLogger logs one debug record per attempt: method, path (without
// query), status, attempt and request ID. Headers and bodies are never
// logged, as they carry tokens and personal data.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) error {
		c.logger = l
		return nil
	}
}

// NewClient returns a client for the API Pix rooted at baseURL, e.g.
// "https://pix.example.com/api/v2". Paths such as "/cob/{txid}" are
// appended to it.
func NewClient(baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("pixapi: base URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("pixapi: base URL must be absolute http(s), got %q", baseURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	c := &Client{
		base:        u,
		retry:       DefaultRetryPolicy,
		timeout:     30 * time.Second,
		reqIDHeader: DefaultRequestIDHeader,
		userAgent:   "pix-cobranca-go/" + Version,
		sleep:       sleepCtx,
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if c.hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if c.tlsConfig != nil {
			tr.TLSClientConfig = c.tlsConfig
		}
		c.hc = &http.Client{Transport: tr}
	}
	if c.oauth != nil && c.oauth.HTTPClient == nil {
		c.oauth.HTTPClient = c.hc
	}
	if c.tokens == nil {
		return nil, errors.New("pixapi: no token source: use WithOAuth2 or WithTokenSource")
	}
	return c, nil
}

// BaseURL returns the API root the client was created with.
func (c *Client) BaseURL() string { return c.base.String() }

// HTTPClient returns the underlying HTTP client, which carries the mTLS
// configuration.
func (c *Client) HTTPClient() *http.Client { return c.hc }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff returns the delay before attempt n (1-based retry number).
func (p RetryPolicy) backoff(n int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if p.MaxDelay > 0 && retryAfter > p.MaxDelay {
			return p.MaxDelay
		}
		return retryAfter
	}
	d := p.BaseDelay << (n - 1)
	if d <= 0 || p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	if d <= 0 {
		return 0
	}
	j, err := rand.Int(rand.Reader, big.NewInt(int64(d)+1))
	if err != nil {
		return d
	}
	return time.Duration(j.Int64())
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Do sends a request to path (relative to the base URL) with an optional
// JSON body, and decodes a JSON response into out when out is not nil. It
// applies authentication, retries, request IDs and error mapping, but no
// validation: it is the escape hatch for endpoints or fields this package
// does not model.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("pixapi: encode request: %w", err)
		}
	}
	// path is already escaped by the callers (see seg).
	u := *c.base
	u.RawPath = c.base.EscapedPath() + path
	p, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return fmt.Errorf("pixapi: invalid path %q: %w", path, err)
	}
	u.Path = p
	u.RawQuery = query.Encode()

	reqID := ""
	if c.reqIDHeader != "" {
		reqID = newRequestID()
	}
	attempts := 1
	if idempotent(method) {
		attempts = c.retry.MaxAttempts
	}
	reauthed := false
	var lastErr error
	for attempt := 1; ; attempt++ {
		status, retryAfter, err := c.attempt(ctx, method, &u, path, payload, reqID, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return finalErr(lastErr)
		}
		// A 401 means the request was not processed: refresh the token
		// and try once more, whatever the method.
		if status == http.StatusUnauthorized && !reauthed {
			if inv, ok := c.tokens.(TokenInvalidator); ok {
				reauthed = true
				inv.Invalidate(tokenFromErr(err))
				attempt--
				continue
			}
		}
		var tokErr *tokenError
		if errors.As(err, &tokErr) {
			return tokErr.err
		}
		if attempt >= attempts || (status != 0 && !retryableStatus(status)) {
			return finalErr(lastErr)
		}
		if err := c.sleep(ctx, c.retry.backoff(attempt, retryAfter)); err != nil {
			return finalErr(lastErr)
		}
	}
}

// tokenError wraps a failure to obtain a token, which is not retried here
// (the token source has its own policy).
type tokenError struct{ err error }

func (e *tokenError) Error() string { return e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

// authError carries the token that was rejected so it can be invalidated.
type authError struct {
	e     *Error
	token string
}

func (e *authError) Error() string { return e.e.Error() }
func (e *authError) Unwrap() error { return e.e }

// finalErr strips internal wrappers before an error reaches the caller.
func finalErr(err error) error {
	var ae *authError
	if errors.As(err, &ae) {
		return ae.e
	}
	return err
}

func tokenFromErr(err error) string {
	var ae *authError
	if errors.As(err, &ae) {
		return ae.token
	}
	return ""
}

func (c *Client) attempt(ctx context.Context, method string, u *url.URL, path string, payload []byte, reqID string, out any) (int, time.Duration, error) {
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return 0, 0, &tokenError{err: err}
	}
	actx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(actx, method, u.String(), rd)
	if err != nil {
		return 0, 0, fmt.Errorf("pixapi: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if reqID != "" {
		req.Header.Set(c.reqIDHeader, reqID)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log(ctx, method, path, 0, reqID, start, err)
		return 0, 0, fmt.Errorf("pixapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.log(ctx, method, path, resp.StatusCode, reqID, start, nil)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			return resp.StatusCode, 0, nil
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
			return resp.StatusCode, 0, fmt.Errorf("pixapi: %s %s: decode response: %w", method, path, err)
		}
		return resp.StatusCode, 0, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{StatusCode: resp.StatusCode, Method: method, Path: path, RequestID: reqID}
	if json.Unmarshal(body, &e.Problem) != nil || (e.Problem.Type == "" && e.Problem.Title == "") {
		e.Problem = Problem{}
		if len(body) > 4096 {
			body = body[:4096]
		}
		e.Body = body
	}
	var rerr error = e
	if resp.StatusCode == http.StatusUnauthorized {
		rerr = &authError{e: e, token: tok}
	}
	return resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), rerr
}

func (c *Client) log(ctx context.Context, method, path string, status int, reqID string, start time.Time, err error) {
	if c.logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.String("request_id", reqID),
		slog.Duration("elapsed", time.Since(start)),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	c.logger.LogAttrs(ctx, slog.LevelDebug, "pixapi request", attrs...)
}
