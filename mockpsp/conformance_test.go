package mockpsp_test

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
	"github.com/victorcondackdev/pix-cobranca-go/pixapitest"
)

type env struct {
	psp    *mockpsp.Server
	ts     *httptest.Server
	pki    *devpki.Set
	client *pixapi.Client
	hc     *http.Client
}

func newEnv(t *testing.T, cfg mockpsp.Config) *env {
	t.Helper()
	pki, err := devpki.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequireMTLS = true
	if cfg.WebhookClient == nil {
		cfg.WebhookClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{pki.WebhookClient.TLS},
			RootCAs:      pki.CA.Pool(),
		}}}
	}
	if cfg.WebhookBackoff == 0 {
		cfg.WebhookBackoff = 10 * time.Millisecond
	}
	psp, err := mockpsp.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(psp.Handler())
	ts.TLS = mockpsp.ServerTLSConfig(pki.Server.TLS, pki.CA.Pool())
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // handshake failures are expected in tests
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		psp.Close()
	})
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{pki.Client.TLS}, RootCAs: pki.CA.Pool()}
	client, err := pixapi.NewClient(ts.URL+psp.BasePath(),
		pixapi.WithTLSConfig(tlsCfg),
		pixapi.WithOAuth2(ts.URL+"/oauth/token", "dev-client", "dev-secret", mockpsp.Scopes...),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &env{psp: psp, ts: ts, pki: pki, client: client, hc: client.HTTPClient()}
}

func (e *env) pay(ctx context.Context, txid string) error {
	_, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, txid, nil, "")
	return err
}

// TestConformance runs the reusable suite against the mock over real mTLS.
func TestConformance(t *testing.T) {
	e := newEnv(t, mockpsp.Config{})
	pixapitest.Run(t, e.client.BaseURL(), e.client,
		pixapitest.WithPayer(e.pay),
		pixapitest.WithRawHTTPClient(e.hc),
	)
}
