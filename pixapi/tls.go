package pixapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// LoadClientTLS builds the TLS configuration for mTLS with a PSP: the
// client certificate and key issued (or registered) by the PSP, and,
// optionally, a PEM bundle of CAs to trust for the PSP's server
// certificate. An empty caFile uses the system roots.
func LoadClientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("pixapi: client certificate: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if caFile != "" {
		pool, err := LoadCertPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// LoadCertPool reads a PEM bundle of CA certificates.
func LoadCertPool(file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("pixapi: CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("pixapi: CA bundle: no certificates found in " + file)
	}
	return pool, nil
}
