package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

func TestCertsCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"certs", "-dir", dir, "-host", "psp.local, 10.0.0.1"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !devpki.Exists(dir) || !strings.Contains(out.String(), "wrote") {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"certs", "-dir", dir}, &out, &out); err != nil || !strings.Contains(out.String(), "already present") {
		t.Fatalf("second run: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"certs", "-dir", dir, "-force"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"certs", "-bogus"}, &out, &out); err == nil {
		t.Error("bad flag accepted")
	}
}

func TestMiscCommands(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, &out); err != nil || strings.TrimSpace(out.String()) != "dev" {
		t.Fatalf("version: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"frobnicate"}, &out, &out); err == nil {
		t.Error("unknown command accepted")
	}
	if err := run(context.Background(), []string{"healthcheck", "-url", "https://127.0.0.1:1/healthz", "-certs", t.TempDir()}, &out, &out); err == nil {
		t.Error("healthcheck without CA succeeded")
	}
	if err := run(context.Background(), []string{"serve", "-addr", "256.0.0.1:0"}, &out, &out); err == nil {
		t.Error("bad address accepted")
	}
	if err := run(context.Background(), []string{"-nope"}, &out, &out); err == nil {
		t.Error("bad serve flag accepted")
	}
}

// serveAsync starts "serve" and returns the bound address.
func serveAsync(t *testing.T, args ...string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	addrc := make(chan string, 1)
	onListen = func(a string) { addrc <- a }
	t.Cleanup(func() { onListen = func(string) {} })
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		errc <- run(ctx, append([]string{"serve", "-addr", "127.0.0.1:0"}, args...), &out, &bytes.Buffer{})
	}()
	select {
	case a := <-addrc:
		return a, cancel, errc
	case err := <-errc:
		cancel()
		t.Fatalf("serve: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("serve did not start")
	}
	return "", cancel, errc
}

func TestServeTLS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	t.Setenv("MOCKPSP_CLIENT_SECRET", "from-env")
	addr, cancel, errc := serveAsync(t, "-certs", dir, "-keys", "a@b.com")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"healthcheck", "-url", "https://" + addr + "/healthz", "-certs", dir}, &out, &out); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	pool, _ := devpki.LoadPool(dir)
	cert, _ := devpki.LoadPair(dir, devpki.FileClientCert, devpki.FileClientKey)
	c, err := pixapi.NewClient("https://"+addr+"/api/v2",
		pixapi.WithTLSConfig(&tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}),
		pixapi.WithOAuth2("https://"+addr+"/oauth/token", "dev-client", "from-env"))
	if err != nil {
		t.Fatal(err)
	}
	cob, err := c.CreateCob(context.Background(), "", pixapi.CobRequest{Valor: pixapi.Valor{Original: 100}, Chave: "a@b.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cob.Location, "localhost:") {
		t.Errorf("location = %q", cob.Location)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestServeInsecure(t *testing.T) {
	addr, cancel, errc := serveAsync(t, "-insecure", "-public-url", "https://psp.example")
	defer cancel()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"healthcheck", "-url", "http://" + addr + "/healthz"}, &out, &out); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if err := run(context.Background(), []string{"healthcheck", "-url", "http://" + addr + "/nope"}, &out, &out); err == nil {
		t.Error("healthcheck of a 404 succeeded")
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}
