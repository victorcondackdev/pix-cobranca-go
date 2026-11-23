// Command mockpsp runs the in-memory API Pix reference server.
//
// Usage:
//
//	mockpsp certs [-dir certs] [-host name,...] [-force]
//	mockpsp serve [-addr :8443] [-certs certs] [-public-url URL] ...
//	mockpsp healthcheck [-url https://localhost:8443/healthz] [-certs certs]
//
// "serve" (the default) generates a development PKI in the certs directory
// on first run, then serves the API over mTLS. The client secret can be
// given with MOCKPSP_CLIENT_SECRET instead of a flag, to keep it out of
// process listings.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
)

var version = "dev"

// onListen is called with the bound address; tests use it to find the port.
var onListen = func(string) {}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mockpsp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(ctx, args, stdout, stderr)
	case "certs":
		return certs(args, stdout, stderr)
	case "healthcheck":
		return healthcheck(ctx, args, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want serve, certs, healthcheck or version)", cmd)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func certs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("certs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "certs", "output directory")
	hosts := fs.String("host", "", "extra comma-separated SANs for the server certificates")
	force := fs.Bool("force", false, "overwrite an existing set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if devpki.Exists(*dir) && !*force {
		fmt.Fprintf(stdout, "certificates already present in %s (use -force to regenerate)\n", *dir)
		return nil
	}
	set, err := devpki.NewSet(splitList(*hosts)...)
	if err != nil {
		return err
	}
	if err := set.Write(*dir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote development CA and certificates to %s\n", *dir)
	return nil
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr("MOCKPSP_ADDR", ":8443"), "listen address")
	dir := fs.String("certs", envOr("MOCKPSP_CERTS", "certs"), "certificate directory (generated if missing)")
	hosts := fs.String("host", envOr("MOCKPSP_HOSTS", ""), "extra SANs when generating certificates")
	publicURL := fs.String("public-url", envOr("MOCKPSP_PUBLIC_URL", ""), "public URL, used in payload locations (default https://localhost:<port>)")
	clientID := fs.String("client-id", envOr("MOCKPSP_CLIENT_ID", "dev-client"), "OAuth2 client ID")
	clientSecret := fs.String("client-secret", "", "OAuth2 client secret (default $MOCKPSP_CLIENT_SECRET or dev-secret)")
	keys := fs.String("keys", envOr("MOCKPSP_KEYS", ""), "comma-separated Pix keys owned by the merchant (default: any)")
	insecure := fs.Bool("insecure", false, "serve plain HTTP without mTLS (local experiments only)")
	allowHTTPWebhooks := fs.Bool("allow-http-webhooks", false, "accept http:// webhook URLs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	secret := *clientSecret
	if secret == "" {
		secret = envOr("MOCKPSP_CLIENT_SECRET", "dev-secret")
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	if *publicURL == "" {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		*publicURL = "https://localhost:" + port
	}

	cfg := mockpsp.Config{
		PublicURL:         *publicURL,
		Clients:           map[string]string{*clientID: secret},
		Keys:              splitList(*keys),
		AllowHTTPWebhooks: *allowHTTPWebhooks,
		Logger:            logger,
	}
	var tlsCfg *tls.Config
	if !*insecure {
		generated, err := devpki.Ensure(*dir, splitList(*hosts)...)
		if err != nil {
			return fmt.Errorf("certificates: %w", err)
		}
		if generated {
			logger.Info("generated development certificates", "dir", *dir)
		}
		pool, err := devpki.LoadPool(*dir)
		if err != nil {
			return err
		}
		serverCert, err := devpki.LoadPair(*dir, devpki.FileServerCert, devpki.FileServerKey)
		if err != nil {
			return err
		}
		whCert, err := devpki.LoadPair(*dir, devpki.FileWebhookClientCrt, devpki.FileWebhookClientKey)
		if err != nil {
			return err
		}
		tlsCfg = mockpsp.ServerTLSConfig(serverCert, pool)
		cfg.RequireMTLS = true
		cfg.WebhookClient = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{whCert}, RootCAs: pool, MinVersion: tls.VersionTLS12},
		}}
	}
	psp, err := mockpsp.New(cfg)
	if err != nil {
		return err
	}
	defer psp.Close()

	srv := &http.Server{
		Handler:           psp.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	scheme := "https"
	if tlsCfg == nil {
		scheme = "http"
	}
	fmt.Fprintf(stdout, "mockpsp %s listening on %s://%s (API at %s, token at /oauth/token)\n", version, scheme, ln.Addr(), psp.BasePath())
	onListen(ln.Addr().String())

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func healthcheck(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	u := fs.String("url", "https://localhost:8443/healthz", "health endpoint")
	dir := fs.String("certs", envOr("MOCKPSP_CERTS", "certs"), "certificate directory (for the CA)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tr := &http.Transport{}
	if strings.HasPrefix(*u, "https://") {
		pool, err := devpki.LoadPool(*dir)
		if err != nil {
			return err
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: status %d", resp.StatusCode)
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
