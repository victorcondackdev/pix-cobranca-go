package devpki

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	generated, err := Ensure(dir, "psp.internal", "10.1.2.3")
	if err != nil || !generated {
		t.Fatalf("Ensure: %v %v", generated, err)
	}
	if generated, err := Ensure(dir); err != nil || generated {
		t.Fatalf("second Ensure regenerated: %v %v", generated, err)
	}
	info, err := os.Stat(filepath.Join(dir, FileServerKey))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %v, %v", info.Mode(), err)
	}
	pool, err := LoadPool(dir)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := LoadPair(dir, FileServerCert, FileServerKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "psp.internal", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("server certificate: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "10.1.2.3"}); err != nil {
		t.Errorf("IP SAN: %v", err)
	}
	client, _ := LoadPair(dir, FileClientCert, FileClientKey)
	cl, _ := x509.ParseCertificate(client.Certificate[0])
	if _, err := cl.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("client certificate valid for server auth")
	}
	if _, err := LoadPair(dir, "nope.pem", "nope-key.pem"); err == nil {
		t.Error("missing pair loaded")
	}
	if _, err := LoadPool(t.TempDir()); err == nil {
		t.Error("missing CA loaded")
	}
	empty := t.TempDir()
	_ = os.WriteFile(filepath.Join(empty, FileCA), []byte("x"), 0o600)
	if _, err := LoadPool(empty); err == nil {
		t.Error("empty CA loaded")
	}
	ca, _ := NewCA("x", time.Hour)
	if ca.Pool() == nil {
		t.Error("Pool")
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := Ensure(filepath.Join(file, "sub")); err == nil {
		t.Error("Ensure under a file succeeded")
	}
}
