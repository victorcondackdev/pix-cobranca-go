package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
)

func TestMerchantEndToEnd(t *testing.T) {
	dir := t.TempDir()
	set, err := devpki.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Write(dir); err != nil {
		t.Fatal(err)
	}
	psp, err := mockpsp.New(mockpsp.Config{
		RequireMTLS:    true,
		WebhookBackoff: 10 * time.Millisecond,
		WebhookClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{set.WebhookClient.TLS}, RootCAs: set.CA.Pool(),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer psp.Close()
	ts := httptest.NewUnstartedServer(psp.Handler())
	ts.TLS = mockpsp.ServerTLSConfig(set.Server.TLS, set.CA.Pool())
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.StartTLS()
	defer ts.Close()

	var out bytes.Buffer
	cfg := config{
		PSP: ts.URL, BasePath: "/api/v2", Certs: dir, ClientID: "dev-client", ClientSecret: "dev-secret",
		Chave: "a@b.com", Amount: "12.34", WebhookAddr: "127.0.0.1:0", Simulate: true, Wait: 10 * time.Second,
	}
	if err := run(context.Background(), cfg, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{"webhook registered", "ATIVA, R$ 12.34", "Pix copia e cola", "dynamic=true", "webhook: pix E", "CONCLUIDA"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	for _, bad := range []config{
		{Amount: "12,34"},
		{Amount: "1.00", Certs: t.TempDir()},
	} {
		if err := run(context.Background(), bad, io.Discard); err == nil {
			t.Errorf("run(%+v) succeeded", bad)
		}
	}
	cfg.Simulate = false
	cfg.ClientSecret = "wrong"
	if err := run(context.Background(), cfg, io.Discard); err == nil {
		t.Error("wrong secret accepted")
	}
}

func TestPrintBRCode(t *testing.T) {
	var out bytes.Buffer
	if err := printBRCode(&out, ""); err != nil || !strings.Contains(out.String(), "no pixCopiaECola") {
		t.Errorf("empty payload: %v %q", err, out.String())
	}
	if err := printBRCode(&out, "garbage"); err == nil {
		t.Error("garbage payload accepted")
	}
}
