package mockpsp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

// ServerTLSConfig returns the TLS configuration for serving the mock with
// mTLS. Client certificates are verified against clientCAs when presented
// (tls.VerifyClientCertIfGiven): an untrusted certificate fails the
// handshake, while a missing one reaches the application, which answers
// with a problem document when Config.RequireMTLS is set. That keeps
// /healthz reachable without a certificate.
func ServerTLSConfig(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	}
}

// SimulatePay calls POST /simulate/pay/{txid} on the mock at rootURL (the
// server root, not the API base path). A nil amount pays valor.original.
func SimulatePay(ctx context.Context, hc *http.Client, rootURL, txid string, amount *pixapi.Amount, infoPagador string) (*pixapi.Pix, error) {
	body, err := json.Marshal(PayRequest{Valor: amount, InfoPagador: infoPagador})
	if err != nil {
		return nil, err
	}
	u := strings.TrimSuffix(rootURL, "/") + "/simulate/pay/" + url.PathEscape(txid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated {
		var p pixapi.Problem
		_ = json.Unmarshal(b, &p)
		return nil, &pixapi.Error{Problem: p, StatusCode: resp.StatusCode, Method: http.MethodPost, Path: "/simulate/pay/" + txid}
	}
	var p pixapi.Pix
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("mockpsp: decode pix: %w", err)
	}
	return &p, nil
}
