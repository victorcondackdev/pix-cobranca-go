package pixapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
)

func noSleep(context.Context, time.Duration) error { return nil }

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts = append([]Option{WithTokenSource(StaticToken("tok"))}, opts...)
	c, err := NewClient(srv.URL+"/api/v2/", opts...)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = noSleep
	return c, srv
}

const txid = "abcdefghijklmnopqrstuvwxyz0123"

func cobJSON(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"txid":%q,"revisao":0,"status":"ATIVA","calendario":{"expiracao":3600},"valor":{"original":"1.00"},"chave":"a@b.com"}`, txid)
}

func TestRetryIdempotent(t *testing.T) {
	var n atomic.Int32
	var ids sync.Map
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		ids.Store(r.Header.Get(DefaultRequestIDHeader), true)
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/api/v2/cob/"+txid {
			t.Errorf("request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		cobJSON(w, 200)
	})
	cob, err := c.GetCob(context.Background(), txid)
	if err != nil {
		t.Fatal(err)
	}
	if cob.TxID != txid || cob.Valor.Original != 100 || n.Load() != 3 {
		t.Fatalf("cob=%+v attempts=%d", cob, n.Load())
	}
	count := 0
	ids.Range(func(k, _ any) bool { count++; return k.(string) != "" })
	if count != 1 {
		t.Errorf("request IDs across retries: %d distinct, want 1", count)
	}
}

func TestNoRetryForPOSTAndClientErrors(t *testing.T) {
	var n atomic.Int32
	status := http.StatusServiceUnavailable
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
	})
	req := CobRequest{Valor: Valor{Original: 100}, Chave: "a@b.com"}
	if _, err := c.CreateCob(context.Background(), "", req); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("POST attempted %d times", n.Load())
	}
	n.Store(0)
	status = http.StatusBadRequest
	if _, err := c.GetCob(context.Background(), txid); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("400 attempted %d times", n.Load())
	}
}

func TestRetryTransportErrorAndTimeout(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(100 * time.Millisecond)
		cobJSON(w, 200)
	}, WithTimeout(20*time.Millisecond), WithRetry(RetryPolicy{MaxAttempts: 2}))
	_, err := c.GetCob(context.Background(), txid)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", n.Load())
	}

	c, srv := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	srv.Close()
	if _, err := c.GetCob(context.Background(), txid); err == nil {
		t.Fatal("no error from closed server")
	}
}

func TestRetryContextCancelledDuringBackoff(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.sleep = sleepCtx
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.GetCob(ctx, txid)
	if !errors.Is(err, ErrRateLimited) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestBackoff(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	for n := 1; n < 10; n++ {
		if d := p.backoff(n, 0); d < 0 || d > time.Second {
			t.Errorf("backoff(%d) = %v", n, d)
		}
	}
	if d := p.backoff(1, 3*time.Second); d != time.Second {
		t.Errorf("Retry-After above MaxDelay = %v", d)
	}
	if d := p.backoff(1, 300*time.Millisecond); d != 300*time.Millisecond {
		t.Errorf("Retry-After = %v", d)
	}
	if d := (RetryPolicy{}).backoff(3, 0); d != 0 {
		t.Errorf("zero policy backoff = %v", d)
	}
	if parseRetryAfter("") != 0 || parseRetryAfter("x") != 0 || parseRetryAfter("2") != 2*time.Second {
		t.Error("parseRetryAfter seconds")
	}
	if d := parseRetryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); d < 59*time.Minute {
		t.Errorf("parseRetryAfter date = %v", d)
	}
}

func TestReauthenticateOn401(t *testing.T) {
	ts := newTokenServer(t)
	var seen []string
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"type":"https://pix.bcb.gov.br/api/v2/error/AcessoNegado","title":"Acesso negado","status":401}`)
			return
		}
		cobJSON(w, 200)
	}))
	defer api.Close()
	// PATCH is not retried on failures, but a 401 is always retried once.
	c, err := NewClient(api.URL, WithOAuth2(ts.URL, "id", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateCob(context.Background(), txid, CobPatch{Status: StatusRemovidaPeloUsuarioRecebedor}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[Bearer tok-1 Bearer tok-2]" {
		t.Fatalf("authorizations = %v", seen)
	}

	// A static token cannot be refreshed: the 401 is returned.
	c2, err := NewClient(api.URL, WithTokenSource(StaticToken("tok-1")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c2.GetCob(context.Background(), txid)
	var pe *Error
	if !errors.Is(err, ErrUnauthorized) || !errors.As(err, &pe) || !HasType(err, TypeAcessoNegado) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if _, ok := err.(*Error); !ok {
		t.Errorf("internal wrapper leaked: %T", err)
	}
}

type failingTokens struct{}

func (failingTokens) Token(context.Context) (string, error) {
	return "", errors.New("no token for you")
}

func TestTokenError(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1) }, WithTokenSource(failingTokens{}))
	if _, err := c.GetCob(context.Background(), txid); err == nil || err.Error() != "no token for you" {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Fatal("request sent without token")
	}
}

func TestErrorMapping(t *testing.T) {
	body := `{"type":"https://psp.example/errors/CobNaoEncontrado","title":"Cobrança não encontrada","status":404,"detail":"nada aqui","correlationId":"c1","violacoes":[{"razao":"r1","propriedade":"p1"},{"razao":"r2","propriedade":"p2"}]}`
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(404)
		fmt.Fprint(w, body)
	})
	_, err := c.GetCob(context.Background(), txid)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatal(err)
	}
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || !HasType(err, TypeCobNaoEncontrado) || HasType(err, TypePixNaoEncontrado) {
		t.Error("errors.Is / HasType")
	}
	if pe.CorrelationID != "c1" || len(pe.Violacoes) != 2 || pe.RequestID == "" {
		t.Errorf("problem = %+v", pe)
	}
	want := "pixapi: GET /cob/" + txid + ": 404 Cobrança não encontrada (https://psp.example/errors/CobNaoEncontrado): nada aqui [p1: r1; p2: r2]"
	if pe.Error() != want {
		t.Errorf("Error()\n got %s\nwant %s", pe.Error(), want)
	}

	// Non-problem bodies are kept, truncated.
	c, _ = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		fmt.Fprint(w, strings.Repeat("x", 5000))
	}, WithRetry(RetryPolicy{MaxAttempts: 1}))
	_, err = c.GetCob(context.Background(), txid)
	if !errors.As(err, &pe) || len(pe.Body) != 4096 || pe.Error() != "pixapi: GET /cob/"+txid+": 502 Bad Gateway" {
		t.Fatalf("err = %v, body %d", err, len(pe.Body))
	}
	if HasType(err, TypeCobNaoEncontrado) || HasType(errors.New("x"), TypeCobNaoEncontrado) {
		t.Error("HasType without type")
	}
	for code, sentinel := range map[int]error{400: ErrBadRequest, 401: ErrUnauthorized, 403: ErrForbidden, 404: ErrNotFound, 409: ErrConflict, 410: ErrGone, 429: ErrRateLimited, 503: ErrUnavailable, 500: ErrServer} {
		if !(&Error{StatusCode: code}).Is(sentinel) {
			t.Errorf("%d is not %v", code, sentinel)
		}
	}
	if (&Error{StatusCode: 500}).Is(io.EOF) {
		t.Error("unrelated sentinel matched")
	}
}

func TestDecodeErrorAndNoContent(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(w, "{not json")
	})
	if _, err := c.GetCob(context.Background(), txid); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v", err)
	}
	if err := c.DeleteWebhook(context.Background(), "a@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodGet, "/x", nil, func() {}, nil); err == nil {
		t.Fatal("unencodable body accepted")
	}
	if err := c.Do(context.Background(), http.MethodGet, "/%zz", nil, nil, nil); err == nil {
		t.Fatal("bad path accepted")
	}
}

func TestPathsAndQueries(t *testing.T) {
	var got []string
	var mu sync.Mutex
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cob"):
			fmt.Fprint(w, `{"parametros":{"paginacao":{"paginaAtual":0,"itensPorPagina":1,"quantidadeDePaginas":1}},"cobs":[]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	ctx := context.Background()
	if err := c.PutWebhook(ctx, "Fulano@Example.com", "https://merchant.example/hook"); err != nil {
		t.Fatal(err)
	}
	if err := c.PutWebhook(ctx, "+55 21 98765-4321", "https://merchant.example/hook"); err != nil {
		t.Fatal(err)
	}
	yes := true
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	it := c.ListCob(ctx, CobQuery{PageQuery: PageQuery{Inicio: start, Fim: start.Add(time.Hour), ItensPorPagina: 1}, CPF: "12345678909", Status: StatusAtiva, LocationPresente: &yes})
	if it.Next() || it.Err() != nil {
		t.Fatalf("empty listing: %v", it.Err())
	}
	if _, err := c.GetCobRevision(ctx, txid, 2); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /api/v2/webhook/fulano@example.com?",
		"PUT /api/v2/webhook/+5521987654321?",
		"GET /api/v2/cob?cpf=12345678909&fim=2026-01-01T01%3A00%3A00Z&inicio=2026-01-01T00%3A00%3A00Z&locationPresente=true&paginacao.itensPorPagina=1&paginacao.paginaAtual=0&status=ATIVA",
		"GET /api/v2/cob/" + txid + "?revisao=2",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLocalValidationSendsNothing(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1) })
	ctx := context.Background()
	valid := CobRequest{Valor: Valor{Original: 100}, Chave: "a@b.com"}
	calls := map[string]func() error{
		"CreateCob txid":     func() error { _, err := c.CreateCob(ctx, "short", valid); return err },
		"CreateCob body":     func() error { _, err := c.CreateCob(ctx, txid, CobRequest{}); return err },
		"GetCob":             func() error { _, err := c.GetCob(ctx, "bad-txid"); return err },
		"UpdateCob txid":     func() error { _, err := c.UpdateCob(ctx, "", CobPatch{}); return err },
		"UpdateCob body":     func() error { _, err := c.UpdateCob(ctx, txid, CobPatch{Status: StatusAtiva}); return err },
		"CreateCobV txid":    func() error { _, err := c.CreateCobV(ctx, "x", CobVRequest{}); return err },
		"CreateCobV body":    func() error { _, err := c.CreateCobV(ctx, txid, CobVRequest{Devedor: &Devedor{}}); return err },
		"GetCobV":            func() error { _, err := c.GetCobV(ctx, "x y"); return err },
		"UpdateCobV txid":    func() error { _, err := c.UpdateCobV(ctx, "x y", CobVPatch{}); return err },
		"UpdateCobV body":    func() error { _, err := c.UpdateCobV(ctx, txid, CobVPatch{Devedor: &Devedor{CPF: "1"}}); return err },
		"GetPix":             func() error { _, err := c.GetPix(ctx, "E1"); return err },
		"CreateDevolucao e2": func() error { _, err := c.CreateDevolucao(ctx, "E1", "d1", DevolucaoRequest{Valor: 1}); return err },
		"CreateDevolucao id": func() error {
			_, err := c.CreateDevolucao(ctx, "E12345678202611221530aB3dE5fG7hJ", "", DevolucaoRequest{Valor: 1})
			return err
		},
		"CreateDevolucao body": func() error {
			_, err := c.CreateDevolucao(ctx, "E12345678202611221530aB3dE5fG7hJ", "d1", DevolucaoRequest{})
			return err
		},
		"GetDevolucao e2": func() error { _, err := c.GetDevolucao(ctx, "E1", "d1"); return err },
		"GetDevolucao id": func() error { _, err := c.GetDevolucao(ctx, "E12345678202611221530aB3dE5fG7hJ", "d-1"); return err },
		"PutWebhook key":  func() error { return c.PutWebhook(ctx, "nope", "https://x.example") },
		"PutWebhook http": func() error { return c.PutWebhook(ctx, "a@b.com", "http://x.example") },
		"PutWebhook rel":  func() error { return c.PutWebhook(ctx, "a@b.com", "/hook") },
		"GetWebhook":      func() error { _, err := c.GetWebhook(ctx, "nope"); return err },
		"DeleteWebhook":   func() error { return c.DeleteWebhook(ctx, "nope") },
		"CreateLoc":       func() error { _, err := c.CreateLoc(ctx, "boleto"); return err },
		"Lote id":         func() error { return c.CreateLoteCobV(ctx, -1, LoteCobVRequest{}) },
		"Lote body":       func() error { return c.UpdateLoteCobV(ctx, 1, LoteCobVRequest{}) },
		"ListCob":         func() error { return c.ListCob(ctx, CobQuery{}).Err() },
		"ListCobV":        func() error { return c.ListCobV(ctx, CobQuery{}).Err() },
		"ListPix":         func() error { return c.ListPix(ctx, PixQuery{}).Err() },
		"ListLoc":         func() error { return c.ListLoc(ctx, LocQuery{}).Err() },
		"ListLote":        func() error { return c.ListLoteCobV(ctx, PageQuery{}).Err() },
		"ListWebhooks range": func() error {
			now := time.Now()
			return c.ListWebhooks(ctx, PageQuery{Inicio: now, Fim: now.Add(-time.Hour), PaginaAtual: -1, ItensPorPagina: -1}).Err()
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests sent for invalid input", n.Load())
	}
}

func TestIteratorPagesAndErrors(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var page int
		fmt.Sscan(r.URL.Query().Get("paginacao.paginaAtual"), &page)
		if n.Add(1) == 3 {
			w.WriteHeader(500)
			return
		}
		fmt.Fprintf(w, `{"parametros":{"paginacao":{"paginaAtual":%d,"itensPorPagina":2,"quantidadeDePaginas":5,"quantidadeTotalDeItens":10}},"loc":[{"id":%d},{"id":%d}]}`, page, page*10+1, page*10+2)
	}, WithRetry(RetryPolicy{MaxAttempts: 1}))
	now := time.Now()
	it := c.ListLoc(context.Background(), LocQuery{PageQuery: PageQuery{Inicio: now.Add(-time.Hour), Fim: now}})
	items, err := it.All()
	if !errors.Is(err, ErrServer) || len(items) != 4 || items[3].ID != 12 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if it.Next() {
		t.Error("Next after error")
	}
	if it.Paginacao().QuantidadeDePaginas != 5 {
		t.Errorf("Paginacao = %+v", it.Paginacao())
	}
}

func TestLogger(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { cobJSON(w, 200) }, WithLogger(logger), WithRequestIDHeader("X-Correlation-Id"), WithUserAgent("test/1"))
	if _, err := c.GetCob(context.Background(), txid); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	_, _ = c.GetCob(context.Background(), txid)
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "status=200") || !strings.Contains(out, "request_id=") || !strings.Contains(out, "error=") {
		t.Errorf("log = %s", out)
	}
	if strings.Contains(out, "Bearer") || strings.Contains(out, "tok") && !strings.Contains(out, "pixapi") {
		t.Errorf("log leaks credentials: %s", out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNewClientOptions(t *testing.T) {
	bad := []struct {
		url  string
		opts []Option
	}{
		{"://x", nil},
		{"ftp://x", []Option{WithTokenSource(StaticToken("t"))}},
		{"https://psp.example", nil},
		{"https://psp.example", []Option{WithHTTPClient(nil)}},
		{"https://psp.example", []Option{WithTLSConfig(nil)}},
		{"https://psp.example", []Option{WithOAuth2("", "", "")}},
	}
	for _, b := range bad {
		if _, err := NewClient(b.url, b.opts...); err == nil {
			t.Errorf("NewClient(%q) succeeded", b.url)
		}
	}
	hc := &http.Client{}
	c, err := NewClient("https://psp.example/api/v2/?x=1", WithHTTPClient(hc), WithOAuth2("https://psp.example/token", "id", "s"), WithRetry(RetryPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != "https://psp.example/api/v2" || c.HTTPClient() != hc || c.oauth.HTTPClient != hc || c.retry.MaxAttempts != 1 {
		t.Errorf("client = %+v", c)
	}
	c, err = NewClient("https://psp.example", WithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS10}), WithTokenSource(StaticToken("t")))
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.hc.Transport.(*http.Transport); tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("MinVersion not raised")
	}
}

// ---- mTLS ------------------------------------------------------------------

func TestMTLSHandshake(t *testing.T) {
	pki, err := devpki.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := pki.Write(dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			t.Error("no peer certificate")
		}
		cobJSON(w, 200)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.Server.TLS}, ClientCAs: pki.CA.Pool(), ClientAuth: tls.RequireAndVerifyClientCert}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake failures
	srv.StartTLS()
	defer srv.Close()

	client := func(t *testing.T, cfg *tls.Config) *Client {
		c, err := NewClient(srv.URL, WithTLSConfig(cfg), WithTokenSource(StaticToken("t")), WithRetry(RetryPolicy{MaxAttempts: 1}))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("success", func(t *testing.T) {
		cfg, err := LoadClientTLS(filepath.Join(dir, devpki.FileClientCert), filepath.Join(dir, devpki.FileClientKey), filepath.Join(dir, devpki.FileCA))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client(t, cfg).GetCob(context.Background(), txid); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no client certificate", func(t *testing.T) {
		_, err := client(t, &tls.Config{RootCAs: pki.CA.Pool()}).GetCob(context.Background(), txid)
		if err == nil {
			t.Fatal("handshake without a client certificate succeeded")
		}
	})
	t.Run("untrusted client certificate", func(t *testing.T) {
		other, err := devpki.NewSet()
		if err != nil {
			t.Fatal(err)
		}
		_, err = client(t, &tls.Config{RootCAs: pki.CA.Pool(), Certificates: []tls.Certificate{other.Client.TLS}}).GetCob(context.Background(), txid)
		if err == nil {
			t.Fatal("handshake with a certificate from another CA succeeded")
		}
	})
	t.Run("untrusted server", func(t *testing.T) {
		other, _ := devpki.NewSet()
		_, err := client(t, &tls.Config{RootCAs: other.CA.Pool(), Certificates: []tls.Certificate{pki.Client.TLS}}).GetCob(context.Background(), txid)
		var ue x509.UnknownAuthorityError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v, want unknown authority", err)
		}
	})
	t.Run("load errors", func(t *testing.T) {
		if _, err := LoadClientTLS("missing.pem", "missing-key.pem", ""); err == nil {
			t.Error("missing files accepted")
		}
		cert, key := filepath.Join(dir, devpki.FileClientCert), filepath.Join(dir, devpki.FileClientKey)
		if _, err := LoadClientTLS(cert, key, "missing-ca.pem"); err == nil {
			t.Error("missing CA accepted")
		}
		empty := filepath.Join(t.TempDir(), "empty.pem")
		_ = os.WriteFile(empty, []byte("nothing"), 0o600)
		if _, err := LoadCertPool(empty); err == nil {
			t.Error("empty CA bundle accepted")
		}
		if cfg, err := LoadClientTLS(cert, key, ""); err != nil || cfg.RootCAs != nil {
			t.Errorf("system roots: %v", err)
		}
	})
}

func TestEndpointsDecode(t *testing.T) {
	// Every typed method decodes its response; the payloads are minimal.
	responses := [][2]string{
		{"GET /pix/E12345678202611221530aB3dE5fG7hJ/devolucao/", `{"id":"d1","rtrId":"D1","valor":"0.50","status":"DEVOLVIDO","horario":{}}`},
		{"GET /pix/", `{"endToEndId":"E12345678202611221530aB3dE5fG7hJ","valor":"1.00","horario":"2026-11-22T10:00:00Z"}`},
		{"PUT /pix/", `{"id":"d1","rtrId":"D1","valor":"0.50","status":"EM_PROCESSAMENTO","horario":{}}`},
		{"GET /webhook/", `{"webhookUrl":"https://x","chave":"a@b.com"}`},
		{"POST /loc", `{"id":7,"location":"x/qr/v2/1","tipoCob":"cob"}`},
		{"GET /loc/", `{"id":7,"location":"x/qr/v2/1","tipoCob":"cob"}`},
		{"DELETE /loc/", `{"id":7,"location":"x/qr/v2/1","tipoCob":"cob"}`},
		{"GET /lotecobv/", `{"descricao":"d","cobsv":[{"txid":"t","status":"CRIADA"}]}`},
		{"PUT /cobv/", `{"txid":"t","status":"ATIVA","calendario":{"dataDeVencimento":"2026-12-31"},"valor":{"original":"1.00"},"chave":"a@b.com"}`},
		{"GET /cobv/", `{"txid":"t","status":"ATIVA","calendario":{"dataDeVencimento":"2026-12-31"},"valor":{"original":"1.00"},"chave":"a@b.com"}`},
		{"PATCH /cobv/", `{"txid":"t","revisao":1,"status":"ATIVA","calendario":{"dataDeVencimento":"2026-12-31"},"valor":{"original":"1.00"},"chave":"a@b.com"}`},
		{"PUT /lotecobv/", ``},
		{"PATCH /lotecobv", ``},
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v2")
		for _, kv := range responses {
			k, v := kv[0], kv[1]
			m, p, _ := strings.Cut(k, " ")
			if r.Method == m && strings.HasPrefix(path, p) {
				if v == "" {
					w.WriteHeader(http.StatusAccepted)
				}
				fmt.Fprint(w, v)
				return
			}
		}
		t.Errorf("unexpected %s %s", r.Method, path)
		w.WriteHeader(404)
	})
	ctx := context.Background()
	e2e := "E12345678202611221530aB3dE5fG7hJ"
	check := func(name string, v any, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		if b, _ := json.Marshal(v); len(b) < 3 {
			t.Errorf("%s: empty result", name)
		}
	}
	p, err := c.GetPix(ctx, e2e)
	check("GetPix", p, err)
	d, err := c.CreateDevolucao(ctx, e2e, "d1", DevolucaoRequest{Valor: 50})
	check("CreateDevolucao", d, err)
	d, err = c.GetDevolucao(ctx, e2e, "d1")
	check("GetDevolucao", d, err)
	wh, err := c.GetWebhook(ctx, "a@b.com")
	check("GetWebhook", wh, err)
	l, err := c.CreateLoc(ctx, TipoCobImediata)
	check("CreateLoc", l, err)
	l, err = c.GetLoc(ctx, 7)
	check("GetLoc", l, err)
	l, err = c.UnlinkLoc(ctx, 7)
	check("UnlinkLoc", l, err)
	lote, err := c.GetLoteCobV(ctx, 1)
	check("GetLoteCobV", lote, err)
	cv := CobVRequest{Calendario: CalendarioCobV{DataDeVencimento: "2026-12-31"}, Devedor: &Devedor{CPF: "12345678909", Nome: "F"}, Valor: ValorCobV{Original: 100}, Chave: "a@b.com"}
	v, err := c.CreateCobV(ctx, txid, cv)
	check("CreateCobV", v, err)
	v, err = c.GetCobV(ctx, txid)
	check("GetCobV", v, err)
	v, err = c.UpdateCobV(ctx, txid, CobVPatch{Devedor: &Devedor{CPF: "12345678909", Nome: "F"}})
	check("UpdateCobV", v, err)
	lr := LoteCobVRequest{CobsV: []LoteCobVRequestItem{{CobVRequest: cv, TxID: txid}}}
	check("CreateLoteCobV", lr, c.CreateLoteCobV(ctx, 1, lr))
	check("UpdateLoteCobV", lr, c.UpdateLoteCobV(ctx, 1, lr))
	if cv.Devedor.CPF != "12345678909" {
		t.Error("caller's request was modified")
	}

	// Error paths of the same methods.
	c, _ = newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	errs := []error{}
	_, err = c.GetPix(ctx, e2e)
	errs = append(errs, err)
	_, err = c.CreateDevolucao(ctx, e2e, "d1", DevolucaoRequest{Valor: 50})
	errs = append(errs, err)
	_, err = c.GetDevolucao(ctx, e2e, "d1")
	errs = append(errs, err)
	_, err = c.GetWebhook(ctx, "a@b.com")
	errs = append(errs, err)
	_, err = c.CreateLoc(ctx, TipoCobVencimento)
	errs = append(errs, err)
	_, err = c.GetLoc(ctx, 1)
	errs = append(errs, err)
	_, err = c.UnlinkLoc(ctx, 1)
	errs = append(errs, err)
	_, err = c.GetLoteCobV(ctx, 1)
	errs = append(errs, err)
	_, err = c.CreateCobV(ctx, txid, cv)
	errs = append(errs, err)
	_, err = c.GetCobV(ctx, txid)
	errs = append(errs, err)
	_, err = c.UpdateCobV(ctx, txid, CobVPatch{})
	errs = append(errs, err)
	_, err = c.CreateCob(ctx, txid, CobRequest{Valor: Valor{Original: 1}, Chave: "a@b.com", Devedor: &Devedor{CPF: "12345678909", Nome: "F"}})
	errs = append(errs, err)
	_, err = c.UpdateCob(ctx, txid, CobPatch{Devedor: &Devedor{CPF: "12345678909", Nome: "F"}})
	errs = append(errs, err)
	for i, err := range errs {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("call %d: err = %v", i, err)
		}
	}
}
