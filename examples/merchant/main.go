// Command merchant is an end-to-end example of a merchant integration:
// it starts an mTLS webhook receiver, registers it with the PSP, creates
// an immediate charge, prints the "copia e cola" payload and its decoded
// fields, and, against mockpsp, simulates the payment and waits for the
// webhook.
//
//	go run ./cmd/mockpsp certs
//	go run ./cmd/mockpsp serve &
//	go run ./examples/merchant -simulate
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/victorcondackdev/brpay/pix"
	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
	"github.com/victorcondackdev/pix-cobranca-go/webhook"
)

type config struct {
	PSP          string // server root, e.g. https://localhost:8443
	BasePath     string
	Certs        string
	ClientID     string
	ClientSecret string
	Chave        string
	Amount       string
	WebhookAddr  string
	WebhookURL   string
	Simulate     bool
	Wait         time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.PSP, "psp", "https://localhost:8443", "PSP root URL")
	flag.StringVar(&cfg.BasePath, "base-path", "/api/v2", "API base path")
	flag.StringVar(&cfg.Certs, "certs", "certs", "directory written by `mockpsp certs`")
	flag.StringVar(&cfg.ClientID, "client-id", "dev-client", "OAuth2 client ID")
	flag.StringVar(&cfg.Chave, "chave", "2c3e8d1a-5b6f-4a7e-9c0d-1e2f3a4b5c6d", "receiver Pix key")
	flag.StringVar(&cfg.Amount, "amount", "12.34", "charge amount")
	flag.StringVar(&cfg.WebhookAddr, "webhook-addr", "127.0.0.1:9443", "webhook listen address")
	flag.StringVar(&cfg.WebhookURL, "webhook-url", "", "webhook URL registered at the PSP (default https://<webhook-addr>/webhook)")
	flag.BoolVar(&cfg.Simulate, "simulate", false, "pay the charge through mockpsp's /simulate/pay and wait for the webhook")
	flag.DurationVar(&cfg.Wait, "wait", 15*time.Second, "how long to wait for the webhook")
	flag.Parse()
	cfg.ClientSecret = os.Getenv("PIX_CLIENT_SECRET")
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = "dev-secret"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "merchant:", err)
		os.Exit(1)
	}
}

func newTxID() string {
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

func run(ctx context.Context, cfg config, out io.Writer) error {
	amount, err := pixapi.ParseAmount(cfg.Amount)
	if err != nil {
		return err
	}
	pool, err := devpki.LoadPool(cfg.Certs)
	if err != nil {
		return err
	}
	clientCert, err := devpki.LoadPair(cfg.Certs, devpki.FileClientCert, devpki.FileClientKey)
	if err != nil {
		return err
	}
	whCert, err := devpki.LoadPair(cfg.Certs, devpki.FileWebhookCert, devpki.FileWebhookKey)
	if err != nil {
		return err
	}

	// 1. Webhook receiver, authenticated by the PSP's client certificate.
	received := make(chan pixapi.Pix, 4)
	h, err := webhook.New(webhook.Config{
		ClientCAs: pool,
		OnPix: func(_ context.Context, p pixapi.Pix) error {
			received <- p
			return nil
		},
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /webhook", h)
	mux.Handle("POST /webhook/pix", h) // the PSP appends "/pix"
	ln, err := net.Listen("tcp", cfg.WebhookAddr)
	if err != nil {
		return err
	}
	srv := webhook.NewServer(ln.Addr().String(), mux, whCert, pool)
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()
	if cfg.WebhookURL == "" {
		cfg.WebhookURL = "https://" + ln.Addr().String() + "/webhook"
	}

	// 2. API client with mTLS and OAuth2 client credentials.
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{clientCert}, RootCAs: pool, MinVersion: tls.VersionTLS12}
	root := strings.TrimSuffix(cfg.PSP, "/")
	client, err := pixapi.NewClient(root+cfg.BasePath,
		pixapi.WithTLSConfig(tlsCfg),
		pixapi.WithOAuth2(root+"/oauth/token", cfg.ClientID, cfg.ClientSecret, "cob.write", "cob.read", "webhook.write", "pix.read"),
	)
	if err != nil {
		return err
	}

	// 3. Register the webhook and create the charge.
	if err := client.PutWebhook(ctx, cfg.Chave, cfg.WebhookURL); err != nil {
		return fmt.Errorf("register webhook: %w", err)
	}
	fmt.Fprintf(out, "webhook registered: %s\n", cfg.WebhookURL)

	cob, err := client.CreateCob(ctx, newTxID(), pixapi.CobRequest{
		Calendario:         &pixapi.Calendario{Expiracao: 3600},
		Devedor:            &pixapi.Devedor{CPF: "123.456.789-09", Nome: "Fulano de Tal"},
		Valor:              pixapi.Valor{Original: amount},
		Chave:              cfg.Chave,
		SolicitacaoPagador: "Pedido 42",
	})
	if err != nil {
		return fmt.Errorf("create charge: %w", err)
	}
	fmt.Fprintf(out, "charge %s: %s, R$ %s\n", cob.TxID, cob.Status, cob.Valor.Original)
	if err := printBRCode(out, cob.PixCopiaECola); err != nil {
		return err
	}

	if !cfg.Simulate {
		return nil
	}

	// 4. Simulate the payer and wait for the notification.
	if _, err := mockpsp.SimulatePay(ctx, client.HTTPClient(), root, cob.TxID, nil, "obrigado"); err != nil {
		return fmt.Errorf("simulate payment: %w", err)
	}
	select {
	case p := <-received:
		fmt.Fprintf(out, "webhook: pix %s received for txid %s, R$ %s\n", p.EndToEndID, p.TxID, p.Valor)
	case <-time.After(cfg.Wait):
		return errors.New("no webhook received")
	case <-ctx.Done():
		return ctx.Err()
	}
	paid, err := client.GetCob(ctx, cob.TxID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "charge %s: %s\n", paid.TxID, paid.Status)
	return nil
}

// printBRCode shows the payload as text: the line to paste in a banking
// app, then what a banking app will read from it.
func printBRCode(out io.Writer, payload string) error {
	if payload == "" {
		fmt.Fprintln(out, "the PSP returned no pixCopiaECola")
		return nil
	}
	bc, err := pix.Decode(payload)
	if err != nil {
		return fmt.Errorf("decode pixCopiaECola: %w", err)
	}
	fmt.Fprintf(out, "\nPix copia e cola:\n\n  %s\n\n", payload)
	fmt.Fprintf(out, "  type:      dynamic=%t reusable=%t\n", bc.Dynamic, bc.Reusable)
	fmt.Fprintf(out, "  location:  %s\n", bc.URL)
	fmt.Fprintf(out, "  merchant:  %s, %s\n", bc.MerchantName, bc.MerchantCity)
	fmt.Fprintf(out, "  crc:       %s\n", bc.CRC)
	for _, is := range pix.Lint(payload) {
		fmt.Fprintf(out, "  lint:      %s\n", is)
	}
	fmt.Fprintln(out)
	return nil
}
