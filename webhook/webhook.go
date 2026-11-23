// Package webhook receives API Pix notifications.
//
// When a charge is paid (or a refund changes state) the PSP sends a POST
// with a body such as
//
//	{"pix": [{"endToEndId": "E...", "txid": "...", "valor": "10.00", "horario": "..."}]}
//
// to the URL registered with PUT /webhook/{chave}. The specification has
// the PSP append "/pix" to the registered URL (verify against the current
// BCB spec version and your PSP's documentation), and the call is made with
// mutual TLS: the PSP presents a client certificate, which is the only
// thing that authenticates the notification. There is no payload
// signature.
//
// Handler verifies that certificate against a configured CA pool, parses
// the payload, drops notifications already processed and hands each new
// Pix to a callback. Answer 2xx only once the notification is durably
// handled: on error the handler answers 500 and the PSP redelivers.
//
// Handler re-verifies the peer chain itself instead of trusting the TLS
// layer alone, so a server misconfigured with tls.RequestClientCert (or
// sharing a listener with other routes) still rejects unauthenticated
// calls. If TLS is terminated by a proxy in front of the application the
// certificate is not visible here; terminate mTLS in the application or
// make the proxy enforce it.
package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

// Notification is the body of a webhook call.
type Notification struct {
	Pix []pixapi.Pix `json:"pix"`
}

// Config configures a Handler.
type Config struct {
	// ClientCAs verifies the PSP's client certificate. Required.
	ClientCAs *x509.CertPool
	// VerifyPeer, when set, runs after chain verification with the leaf
	// certificate, e.g. to pin the PSP's subject or SPKI hash.
	VerifyPeer func(leaf *x509.Certificate) error
	// OnPix is called once per new Pix. Required. A returned error makes
	// the handler answer 500 so the PSP retries.
	OnPix func(ctx context.Context, p pixapi.Pix) error
	// Dedupe tracks processed notifications. Nil means a MemoryDeduper
	// keeping keys for 72 hours, which is only adequate for a single
	// instance: use a shared store when running several.
	Dedupe Deduper
	// MaxBodyBytes bounds the request body; zero means 1 MiB.
	MaxBodyBytes int64
	// Logger receives one record per rejected or failed call. Payloads
	// are never logged.
	Logger *slog.Logger
}

// Handler is an http.Handler for API Pix webhooks.
type Handler struct {
	cfg Config
	now func() time.Time
}

// New validates cfg and returns a Handler.
func New(cfg Config) (*Handler, error) {
	if cfg.ClientCAs == nil {
		return nil, errors.New("webhook: ClientCAs is required")
	}
	if cfg.OnPix == nil {
		return nil, errors.New("webhook: OnPix is required")
	}
	if cfg.Dedupe == nil {
		cfg.Dedupe = NewMemoryDeduper(72*time.Hour, 1_000_000)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	return &Handler{cfg: cfg, now: time.Now}, nil
}

// Key is the deduplication key of a notification: the EndToEndId, plus the
// id and status of each devolução, so that refund updates for an
// already-notified Pix are delivered rather than dropped.
func Key(p pixapi.Pix) string {
	if len(p.Devolucoes) == 0 {
		return p.EndToEndID
	}
	parts := make([]string, len(p.Devolucoes))
	for i, d := range p.Devolucoes {
		parts[i] = d.ID + ":" + string(d.Status)
	}
	sort.Strings(parts)
	return p.EndToEndID + "|" + strings.Join(parts, ",")
}

// ErrNoPeerCertificate is returned by VerifyPeer when the connection has
// no client certificate.
var ErrNoPeerCertificate = errors.New("webhook: no client certificate")

// VerifyPeerChain verifies the client certificate of a TLS connection
// against roots, for client authentication, at time now.
func VerifyPeerChain(state *tls.ConnectionState, roots *x509.CertPool, now time.Time) (*x509.Certificate, error) {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil, ErrNoPeerCertificate
	}
	leaf := state.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range state.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: client certificate: %w", err)
	}
	return leaf, nil
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request, code int, msg string, err error) {
	if h.cfg.Logger != nil {
		attrs := []any{slog.Int("status", code), slog.String("remote", r.RemoteAddr)}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		h.cfg.Logger.Warn("pix webhook rejected", attrs...)
	}
	http.Error(w, msg, code)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.reject(w, r, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	leaf, err := VerifyPeerChain(r.TLS, h.cfg.ClientCAs, h.now())
	if err != nil {
		code := http.StatusForbidden
		if errors.Is(err, ErrNoPeerCertificate) {
			code = http.StatusUnauthorized
		}
		h.reject(w, r, code, "client certificate required", err)
		return
	}
	if h.cfg.VerifyPeer != nil {
		if err := h.cfg.VerifyPeer(leaf); err != nil {
			h.reject(w, r, http.StatusForbidden, "client certificate not allowed", err)
			return
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes))
	if err != nil {
		h.reject(w, r, http.StatusRequestEntityTooLarge, "body too large", err)
		return
	}
	var n Notification
	if err := json.Unmarshal(body, &n); err != nil {
		h.reject(w, r, http.StatusBadRequest, "invalid payload", err)
		return
	}
	for i, p := range n.Pix {
		if p.EndToEndID == "" {
			h.reject(w, r, http.StatusBadRequest, "invalid payload", fmt.Errorf("pix[%d] without endToEndId", i))
			return
		}
	}

	ctx := r.Context()
	for _, p := range n.Pix {
		key := Key(p)
		proceed, err := h.cfg.Dedupe.Begin(ctx, key)
		if err != nil {
			h.reject(w, r, http.StatusInternalServerError, "temporarily unavailable", err)
			return
		}
		if !proceed {
			continue
		}
		if err := h.cfg.OnPix(ctx, p); err != nil {
			_ = h.cfg.Dedupe.Abort(ctx, key)
			h.reject(w, r, http.StatusInternalServerError, "processing failed", err)
			return
		}
		if err := h.cfg.Dedupe.Commit(ctx, key); err != nil {
			h.reject(w, r, http.StatusInternalServerError, "temporarily unavailable", err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// TLSConfig returns a server TLS configuration that requires and verifies
// a client certificate issued by clientCAs, with TLS 1.2 as the minimum.
func TLSConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
}

// NewServer returns an *http.Server for a webhook receiver, with mTLS and
// conservative timeouts. Start it with ListenAndServeTLS("", "").
func NewServer(addr string, h http.Handler, serverCert tls.Certificate, clientCAs *x509.CertPool) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         TLSConfig(serverCert, clientCAs),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}
