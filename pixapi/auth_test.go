package pixapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type tokenServer struct {
	*httptest.Server
	calls   atomic.Int64
	delay   time.Duration
	expires int
	fail    atomic.Bool
	gate    chan struct{}
	lastReq atomic.Value // url.Values + auth
}

func newTokenServer(t *testing.T) *tokenServer {
	ts := &tokenServer{expires: 300}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := ts.calls.Add(1)
		if ts.gate != nil {
			<-ts.gate
		}
		time.Sleep(ts.delay)
		_ = r.ParseForm()
		id, secret, _ := r.BasicAuth()
		ts.lastReq.Store(fmt.Sprintf("grant=%s scope=%s basic=%s:%s form=%s:%s", r.PostForm.Get("grant_type"), r.PostForm.Get("scope"),
			id, secret, r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")))
		w.Header().Set("Content-Type", "application/json")
		if ts.fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_client","error_description":"bad credentials"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"Bearer","expires_in":%d}`, n, ts.expires)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestClientCredentialsCaching(t *testing.T) {
	ts := newTokenServer(t)
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	cc := &ClientCredentials{TokenURL: ts.URL, ClientID: "id", ClientSecret: "s3cret", Scopes: []string{"cob.read", "cob.write"}, now: clk.Now}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		tok, err := cc.Token(ctx)
		if err != nil || tok != "tok-1" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	if got := ts.lastReq.Load().(string); got != "grant=client_credentials scope=cob.read cob.write basic=id:s3cret form=:" {
		t.Errorf("token request = %s", got)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times, want 1", n)
	}

	// Inside the early refresh window: the current token is returned at
	// once and a refresh happens in the background.
	clk.Add(250 * time.Second)
	tok, err := cc.Token(ctx)
	if err != nil || tok != "tok-1" {
		t.Fatalf("Token in refresh window = %q, %v", tok, err)
	}
	waitFor(t, func() bool {
		tok, _ := cc.Token(ctx)
		return tok == "tok-2"
	})

	// Past expiry with no refresh: callers wait for a new token.
	clk.Add(400 * time.Second)
	if tok, _ := cc.Token(ctx); tok != "tok-3" {
		t.Fatalf("Token after expiry = %q, want tok-3", tok)
	}

	// Invalidate drops only the rejected token.
	cc.Invalidate("tok-1")
	if tok, _ := cc.Token(ctx); tok != "tok-3" {
		t.Fatalf("Invalidate of a stale token dropped the current one: %q", tok)
	}
	cc.Invalidate("tok-3")
	if tok, _ := cc.Token(ctx); tok != "tok-4" {
		t.Fatalf("Token after Invalidate = %q, want tok-4", tok)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestClientCredentialsSingleFlight hammers an empty cache from many
// goroutines; run with -race.
func TestClientCredentialsSingleFlight(t *testing.T) {
	ts := newTokenServer(t)
	ts.delay = 50 * time.Millisecond
	cc := &ClientCredentials{TokenURL: ts.URL, ClientID: "id", ClientSecret: "s"}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := cc.Token(context.Background())
			if err == nil && tok != "tok-1" {
				err = fmt.Errorf("token %q", tok)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times, want 1", n)
	}
}

// TestClientCredentialsRefreshRace refreshes concurrently with readers and
// invalidations; run with -race.
func TestClientCredentialsRefreshRace(t *testing.T) {
	ts := newTokenServer(t)
	ts.expires = 1 // one-second tokens: early refresh at 0.5s
	cc := &ClientCredentials{TokenURL: ts.URL, ClientID: "id", ClientSecret: "s"}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for ctx.Err() == nil {
				tok, err := cc.Token(ctx)
				if err != nil && ctx.Err() == nil {
					t.Error(err)
					return
				}
				if i%4 == 0 {
					cc.Invalidate(tok)
				}
			}
		}(i)
	}
	wg.Wait()
	if ts.calls.Load() < 2 {
		t.Errorf("token endpoint called %d times, want refreshes", ts.calls.Load())
	}
}

func TestClientCredentialsCancelledWaiter(t *testing.T) {
	ts := newTokenServer(t)
	ts.gate = make(chan struct{})
	cc := &ClientCredentials{TokenURL: ts.URL, ClientID: "id", ClientSecret: "s"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cc.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token with cancelled context = %v", err)
	}
	// The shared fetch was not cancelled: a later caller gets its result.
	close(ts.gate)
	tok, err := cc.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
}

func TestClientCredentialsErrors(t *testing.T) {
	ts := newTokenServer(t)
	ts.fail.Store(true)
	cc := &ClientCredentials{TokenURL: ts.URL, ClientID: "id", ClientSecret: "s", SecretInBody: true}
	_, err := cc.Token(context.Background())
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_client" || oe.StatusCode != 401 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_client: bad credentials") {
		t.Errorf("message = %q", err)
	}
	if got := ts.lastReq.Load().(string); !strings.Contains(got, "form=id:s") || strings.Contains(got, "basic=id") {
		t.Errorf("SecretInBody request = %s", got)
	}

	// A failed background refresh keeps the valid token.
	ts.fail.Store(false)
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	cc = &ClientCredentials{TokenURL: ts.URL, ClientID: "id", ClientSecret: "s", now: clk.Now, EarlyRefresh: time.Minute}
	first, err := cc.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ts.fail.Store(true)
	clk.Add(250 * time.Second)
	before := ts.calls.Load()
	for i := 0; i < 20; i++ {
		if tok, err := cc.Token(context.Background()); err != nil || tok != first {
			t.Fatalf("Token during failing refresh = %q, %v", tok, err)
		}
	}
	waitFor(t, func() bool { return ts.calls.Load() == before+1 })
	time.Sleep(20 * time.Millisecond)
	if tok, _ := cc.Token(context.Background()); tok != first || ts.calls.Load() != before+1 {
		t.Errorf("refresh retried immediately (%d calls) or token lost (%q)", ts.calls.Load()-before, tok)
	}

	bad := []struct {
		name, body, ctype string
		status            int
	}{
		{"not json", "nope", "text/plain", 200},
		{"no token", `{"token_type":"Bearer"}`, "application/json", 200},
		{"mac token", `{"access_token":"x","token_type":"mac"}`, "application/json", 200},
		{"500 html", "<html>", "text/html", 500},
	}
	for _, b := range bad {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", b.ctype)
			w.WriteHeader(b.status)
			fmt.Fprint(w, b.body)
		}))
		cc := &ClientCredentials{TokenURL: srv.URL, ClientID: "id"}
		if _, err := cc.Token(context.Background()); err == nil {
			t.Errorf("%s: no error", b.name)
		}
		srv.Close()
	}
	cc = &ClientCredentials{TokenURL: "://bad", ClientID: "id"}
	if _, err := cc.Token(context.Background()); err == nil {
		t.Error("bad URL: no error")
	}
	cc = &ClientCredentials{TokenURL: "http://127.0.0.1:1", ClientID: "id"}
	if _, err := cc.Token(context.Background()); err == nil {
		t.Error("unreachable: no error")
	}
}

func TestClientCredentialsDefaultLifetime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"x"}`)
	}))
	defer srv.Close()
	cc := &ClientCredentials{TokenURL: srv.URL, ClientID: "id"}
	if _, err := cc.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(cc.expiry); d < 4*time.Minute || d > 5*time.Minute {
		t.Errorf("default lifetime = %v", d)
	}
}

func TestClientCredentialsRedaction(t *testing.T) {
	cc := &ClientCredentials{TokenURL: "https://psp/token", ClientID: "id", ClientSecret: "top-secret"}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("auth", "cc", cc)
	for _, s := range []string{cc.String(), fmt.Sprint(cc), fmt.Sprintf("%v", cc), buf.String()} {
		if strings.Contains(s, "top-secret") {
			t.Errorf("secret leaked: %s", s)
		}
	}
	if !strings.Contains(buf.String(), "client_id=id") {
		t.Errorf("slog output = %s", buf.String())
	}
	if tok, _ := StaticToken("abc").Token(context.Background()); tok != "abc" {
		t.Error("StaticToken")
	}
}
