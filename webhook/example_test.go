package webhook_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"

	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
	"github.com/victorcondackdev/pix-cobranca-go/webhook"
)

func Example() {
	var (
		pspCAs     *x509.CertPool  // CAs that issue the PSP's client certificate
		serverCert tls.Certificate // this receiver's certificate
	)
	h, err := webhook.New(webhook.Config{
		ClientCAs: pspCAs,
		OnPix: func(ctx context.Context, p pixapi.Pix) error {
			// Persist and reconcile by p.TxID before answering: an error
			// here makes the PSP redeliver.
			log.Printf("received %s for %s: R$ %s", p.EndToEndID, p.TxID, p.Valor)
			return nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /webhook/pix", h) // registered as https://merchant.example/webhook
	srv := webhook.NewServer(":9443", mux, serverCert, pspCAs)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
