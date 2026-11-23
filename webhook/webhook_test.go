package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

const payload = `{"pix":[{"endToEndId":"E12345678202611221530aB3dE5fG7hJ","txid":"abc","valor":"10.00","chave":"a@b.com","horario":"2026-11-22T15:30:00Z"}]}`

type recorder struct {
	mu   sync.Mutex
	got  []pixapi.Pix
	fail atomic.Int32 // number of calls to fail
}

func (r *recorder) onPix(_ context.Context, p pixapi.Pix) error {
	if r.fail.Load() > 0 {
		r.fail.Add(-1)
		return errors.New("database down")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, p)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func newHandler(t *testing.T, pool *x509.CertPool, rec *recorder, mut func(*Config)) *Handler {
	t.Helper()
	cfg := Config{ClientCAs: pool, OnPix: rec.onPix, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mut != nil {
		mut(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(h http.Handler, state *tls.ConnectionState, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/pix", strings.NewReader(body))
	req.TLS = state
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func stateFor(certs ...tls.Certificate) *tls.ConnectionState {
	st := &tls.ConnectionState{}
	for _, c := range certs {
		leaf, _ := x509.ParseCertificate(c.Certificate[0])
		st.PeerCertificates = append(st.PeerCertificates, leaf)
	}
	return st
}

func TestHandler(t *testing.T) {
	pki, err := devpki.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	other, err := devpki.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	h := newHandler(t, pki.CA.Pool(), rec, nil)
	good := stateFor(pki.WebhookClient.TLS)

	if w := post(h, good, payload); w.Code != 200 || rec.count() != 1 {
		t.Fatalf("first delivery: %d, %d calls", w.Code, rec.count())
	}
	if p := rec.got[0]; p.Valor != 1000 || p.TxID != "abc" || p.Horario.IsZero() {
		t.Errorf("parsed pix = %+v", p)
	}
	// Redelivery is acknowledged but not processed again.
	if w := post(h, good, payload); w.Code != 200 || rec.count() != 1 {
		t.Fatalf("redelivery: %d, %d calls", w.Code, rec.count())
	}
	// A refund update of the same Pix is a new notification.
	refund := strings.Replace(payload, `"horario":"2026-11-22T15:30:00Z"`, `"horario":"2026-11-22T15:30:00Z","devolucoes":[{"id":"d1","rtrId":"D1","valor":"1.00","status":"DEVOLVIDO","horario":{}}]`, 1)
	if w := post(h, good, refund); w.Code != 200 || rec.count() != 2 {
		t.Fatalf("refund update: %d, %d calls", w.Code, rec.count())
	}

	cases := []struct {
		name  string
		state *tls.ConnectionState
		body  string
		code  int
	}{
		{"no TLS", nil, payload, 401},
		{"no certificate", &tls.ConnectionState{}, payload, 401},
		{"other CA", stateFor(other.WebhookClient.TLS), payload, 403},
		{"server-auth certificate", stateFor(pki.WebhookServer.TLS), payload, 403},
		{"malformed JSON", good, `{"pix":`, 400},
		{"bad amount", good, `{"pix":[{"endToEndId":"E1","valor":10}]}`, 400},
		{"no endToEndId", good, `{"pix":[{"valor":"1.00"}]}`, 400},
		{"too large", good, `{"pix":[],"pad":"` + strings.Repeat("x", 2<<20) + `"}`, 413},
	}
	for _, tc := range cases {
		if w := post(h, tc.state, tc.body); w.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/webhook/pix", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Errorf("GET: %d", w.Code)
	}
	if w := post(h, good, `{"pix":[]}`); w.Code != 200 {
		t.Errorf("empty notification: %d", w.Code)
	}
}

func TestHandlerCallbackFailureIsRetried(t *testing.T) {
	pki, _ := devpki.NewSet()
	rec := &recorder{}
	rec.fail.Store(1)
	h := newHandler(t, pki.CA.Pool(), rec, nil)
	st := stateFor(pki.WebhookClient.TLS)
	if w := post(h, st, payload); w.Code != 500 {
		t.Fatalf("failing callback: %d", w.Code)
	}
	if w := post(h, st, payload); w.Code != 200 || rec.count() != 1 {
		t.Fatalf("redelivery after failure: %d, %d calls", w.Code, rec.count())
	}
}

func TestHandlerVerifyPeer(t *testing.T) {
	pki, _ := devpki.NewSet()
	rec := &recorder{}
	h := newHandler(t, pki.CA.Pool(), rec, func(c *Config) {
		c.VerifyPeer = func(leaf *x509.Certificate) error {
			if leaf.Subject.CommonName != "expected-psp" {
				return errors.New("unexpected subject " + leaf.Subject.CommonName)
			}
			return nil
		}
	})
	if w := post(h, stateFor(pki.WebhookClient.TLS), payload); w.Code != 403 || rec.count() != 0 {
		t.Fatalf("pinned subject mismatch: %d", w.Code)
	}
}

type brokenDeduper struct{ beginErr, commitErr error }

func (b brokenDeduper) Begin(context.Context, string) (bool, error) {
	return b.beginErr == nil, b.beginErr
}
func (b brokenDeduper) Commit(context.Context, string) error { return b.commitErr }
func (b brokenDeduper) Abort(context.Context, string) error  { return nil }

func TestHandlerDeduperErrors(t *testing.T) {
	pki, _ := devpki.NewSet()
	st := stateFor(pki.WebhookClient.TLS)
	for _, d := range []brokenDeduper{{beginErr: errors.New("redis down")}, {commitErr: errors.New("redis down")}} {
		h := newHandler(t, pki.CA.Pool(), &recorder{}, func(c *Config) { c.Dedupe = d })
		if w := post(h, st, payload); w.Code != 500 {
			t.Errorf("deduper %+v: %d", d, w.Code)
		}
	}
}

func TestNewErrors(t *testing.T) {
	if _, err := New(Config{OnPix: (&recorder{}).onPix}); err == nil {
		t.Error("missing ClientCAs accepted")
	}
	if _, err := New(Config{ClientCAs: x509.NewCertPool()}); err == nil {
		t.Error("missing OnPix accepted")
	}
}

func TestKey(t *testing.T) {
	p := pixapi.Pix{EndToEndID: "E1"}
	if Key(p) != "E1" {
		t.Error(Key(p))
	}
	p.Devolucoes = []pixapi.Devolucao{{ID: "b", Status: pixapi.DevolucaoDevolvido}, {ID: "a", Status: pixapi.DevolucaoEmProcessamento}}
	if Key(p) != "E1|a:EM_PROCESSAMENTO,b:DEVOLVIDO" {
		t.Error(Key(p))
	}
}

func TestMemoryDeduper(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	m := NewMemoryDeduper(time.Minute, 2)
	m.now = func() time.Time { return now }
	if ok, _ := m.Begin(ctx, "a"); !ok {
		t.Fatal("first Begin")
	}
	if ok, _ := m.Begin(ctx, "a"); ok {
		t.Fatal("in-progress key reported as new")
	}
	_ = m.Abort(ctx, "a")
	if ok, _ := m.Begin(ctx, "a"); !ok {
		t.Fatal("aborted key not available again")
	}
	_ = m.Commit(ctx, "a")
	_ = m.Abort(ctx, "a") // no effect once committed
	if ok, _ := m.Begin(ctx, "a"); ok {
		t.Fatal("committed key reported as new")
	}
	m.Begin(ctx, "b")
	m.Begin(ctx, "c") // evicts a (max 2)
	if m.Len() != 2 {
		t.Fatalf("Len = %d", m.Len())
	}
	if ok, _ := m.Begin(ctx, "a"); !ok {
		t.Fatal("evicted key still remembered")
	}
	now = now.Add(2 * time.Minute)
	if ok, _ := m.Begin(ctx, "b"); !ok {
		t.Fatal("expired key still remembered")
	}
	if m.Len() != 1 {
		t.Fatalf("Len after expiry = %d", m.Len())
	}
	_ = m.Commit(ctx, "missing")
	if NewMemoryDeduper(time.Minute, 0).max != 1 {
		t.Error("max not clamped")
	}
}

// TestServerMTLS runs the receiver behind NewServer and checks the TLS
// handshake itself.
func TestServerMTLS(t *testing.T) {
	pki, _ := devpki.NewSet()
	other, _ := devpki.NewSet()
	rec := &recorder{}
	h := newHandler(t, pki.CA.Pool(), rec, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ln.Addr().String(), h, pki.WebhookServer.TLS, pki.CA.Pool())
	srv.ErrorLog = log.New(io.Discard, "", 0)
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()
	url := "https://" + ln.Addr().String() + "/webhook/pix"

	send := func(certs []tls.Certificate) (int, error) {
		c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pki.CA.Pool(), Certificates: certs}}}
		resp, err := c.Post(url, "application/json", bytes.NewBufferString(payload))
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := send([]tls.Certificate{pki.WebhookClient.TLS}); err != nil || code != 200 {
		t.Fatalf("trusted PSP: %d, %v", code, err)
	}
	if _, err := send(nil); err == nil {
		t.Error("handshake without client certificate succeeded")
	}
	if _, err := send([]tls.Certificate{other.WebhookClient.TLS}); err == nil {
		t.Error("handshake with untrusted client certificate succeeded")
	}
	if rec.count() != 1 {
		t.Errorf("callbacks = %d", rec.count())
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 || srv.TLSConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("TLS config")
	}
}
