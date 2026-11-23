// Package devpki generates a throwaway certificate authority and the leaf
// certificates needed to exercise mTLS locally: the PSP server, the
// merchant's API client, the merchant's webhook server and the PSP's
// webhook client. It is for development and tests only.
package devpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CA is a development certificate authority.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

// Leaf is an issued certificate.
type Leaf struct {
	CertPEM, KeyPEM []byte
	TLS             tls.Certificate
}

// NewCA creates a self-signed CA valid for validity.
func NewCA(name string, validity time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"pix-cobranca-go dev"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// Pool returns a pool containing only the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// Issue signs a leaf certificate. Hosts become DNS or IP SANs; usage is
// x509.ExtKeyUsageServerAuth or x509.ExtKeyUsageClientAuth.
func (ca *CA) Issue(cn string, hosts []string, usage x509.ExtKeyUsage, validity time.Duration) (*Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"pix-cobranca-go dev"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	tc, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &Leaf{CertPEM: certPEM, KeyPEM: keyPEM, TLS: tc}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// Set is the full set of development certificates.
type Set struct {
	CA *CA
	// Server is the PSP's API server certificate.
	Server *Leaf
	// Client is the merchant's API client certificate.
	Client *Leaf
	// WebhookServer is the merchant's webhook receiver certificate.
	WebhookServer *Leaf
	// WebhookClient is the certificate the PSP presents to webhooks.
	WebhookClient *Leaf
}

// DefaultHosts are the SANs of the server certificates.
var DefaultHosts = []string{"localhost", "127.0.0.1", "::1", "mockpsp"}

// NewSet generates a CA and all leaves. Extra hosts are added to the
// server certificates' SANs.
func NewSet(extraHosts ...string) (*Set, error) {
	hosts := append(append([]string{}, DefaultHosts...), extraHosts...)
	const validity = 365 * 24 * time.Hour
	ca, err := NewCA("pix-cobranca-go dev CA", validity)
	if err != nil {
		return nil, err
	}
	s := &Set{CA: ca}
	steps := []struct {
		dst   **Leaf
		cn    string
		hosts []string
		usage x509.ExtKeyUsage
	}{
		{&s.Server, "mockpsp", hosts, x509.ExtKeyUsageServerAuth},
		{&s.Client, "merchant-client", nil, x509.ExtKeyUsageClientAuth},
		{&s.WebhookServer, "merchant-webhook", hosts, x509.ExtKeyUsageServerAuth},
		{&s.WebhookClient, "mockpsp-webhook-client", nil, x509.ExtKeyUsageClientAuth},
	}
	for _, st := range steps {
		l, err := ca.Issue(st.cn, st.hosts, st.usage, validity)
		if err != nil {
			return nil, err
		}
		*st.dst = l
	}
	return s, nil
}

// File names written by Write.
const (
	FileCA               = "ca.pem"
	FileServerCert       = "server.pem"
	FileServerKey        = "server-key.pem"
	FileClientCert       = "client.pem"
	FileClientKey        = "client-key.pem"
	FileWebhookCert      = "webhook-server.pem"
	FileWebhookKey       = "webhook-server-key.pem"
	FileWebhookClientCrt = "psp-webhook-client.pem"
	FileWebhookClientKey = "psp-webhook-client-key.pem"
)

// Write stores the set in dir (created with mode 0700). Private keys are
// written with mode 0600. The CA private key is not written: issue a new
// set instead of extending an old one.
func (s *Set) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{FileCA, s.CA.CertPEM, 0o644},
		{FileServerCert, s.Server.CertPEM, 0o644},
		{FileServerKey, s.Server.KeyPEM, 0o600},
		{FileClientCert, s.Client.CertPEM, 0o644},
		{FileClientKey, s.Client.KeyPEM, 0o600},
		{FileWebhookCert, s.WebhookServer.CertPEM, 0o644},
		{FileWebhookKey, s.WebhookServer.KeyPEM, 0o600},
		{FileWebhookClientCrt, s.WebhookClient.CertPEM, 0o644},
		{FileWebhookClientKey, s.WebhookClient.KeyPEM, 0o600},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// Exists reports whether dir holds a complete set.
func Exists(dir string) bool {
	for _, n := range []string{FileCA, FileServerCert, FileServerKey, FileClientCert, FileClientKey, FileWebhookCert, FileWebhookKey, FileWebhookClientCrt, FileWebhookClientKey} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return false
		}
	}
	return true
}

// Ensure generates and writes a set in dir unless one already exists. It
// reports whether it generated one.
func Ensure(dir string, extraHosts ...string) (bool, error) {
	if Exists(dir) {
		return false, nil
	}
	s, err := NewSet(extraHosts...)
	if err != nil {
		return false, err
	}
	return true, s.Write(dir)
}

// LoadPair loads a certificate and key from dir.
func LoadPair(dir, certName, keyName string) (tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(filepath.Join(dir, certName), filepath.Join(dir, keyName))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("devpki: %w", err)
	}
	return c, nil
}

// LoadPool loads the CA from dir.
func LoadPool(dir string) (*x509.CertPool, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileCA))
	if err != nil {
		return nil, fmt.Errorf("devpki: %w", err)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, errors.New("devpki: no certificate in " + FileCA)
	}
	return p, nil
}
