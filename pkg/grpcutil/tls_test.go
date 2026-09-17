package grpcutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

// writeTestPKI generates a CA and a leaf cert (usable as both server and
// client, SAN internal.mavericks + 127.0.0.1) into dir, mirroring the shape
// cert-manager mounts in production: tls.crt / tls.key / ca.crt.
func writeTestPKI(t *testing.T, dir string) (certFile, keyFile, caFile string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "internal.mavericks"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames:    []string{"internal.mavericks", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)

	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	caFile = filepath.Join(dir, "ca.crt")
	mustWrite := func(path, typ string, der []byte) {
		t.Helper()
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	mustWrite(certFile, "CERTIFICATE", leafDER)
	mustWrite(keyFile, "EC PRIVATE KEY", keyDER)
	mustWrite(caFile, "CERTIFICATE", caDER)
	return certFile, keyFile, caFile
}

// TestMTLSRoundTripAndPlaintextRefused is the transport contract: with the
// MTLS_* env set, a client presenting a CA-signed cert completes a health
// check, and a plaintext client is refused at the transport — the enforcement
// that makes in-cluster eavesdropping/joining impossible without the CA.
func TestMTLSRoundTripAndPlaintextRefused(t *testing.T) {
	certFile, keyFile, caFile := writeTestPKI(t, t.TempDir())
	t.Setenv("MTLS_CERT_FILE", certFile)
	t.Setenv("MTLS_KEY_FILE", keyFile)
	t.Setenv("MTLS_CA_FILE", caFile)

	srv := NewServer()
	lc := &net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// mTLS client: succeeds.
	opt, enabled, err := ClientTLSOption()
	if err != nil || !enabled {
		t.Fatalf("ClientTLSOption: enabled=%v err=%v", enabled, err)
	}
	conn, err := grpc.NewClient(lis.Addr().String(), opt)
	if err != nil {
		t.Fatalf("dial mTLS: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if _, err := healthgrpc.NewHealthClient(conn).Check(ctx, &healthgrpc.HealthCheckRequest{}); err != nil {
		t.Fatalf("health check over mTLS: %v", err)
	}

	// Plaintext client: refused at the transport.
	plain, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial plaintext: %v", err)
	}
	defer plain.Close() //nolint:errcheck
	if _, err := healthgrpc.NewHealthClient(plain).Check(ctx, &healthgrpc.HealthCheckRequest{}); err == nil {
		t.Fatal("plaintext health check SUCCEEDED against an mTLS server — enforcement missing")
	}
}

// TestMTLSHalfConfiguredFailsLoudly: two of three vars set must not silently
// fall back to plaintext.
func TestMTLSHalfConfiguredFailsLoudly(t *testing.T) {
	certFile, keyFile, _ := writeTestPKI(t, t.TempDir())
	t.Setenv("MTLS_CERT_FILE", certFile)
	t.Setenv("MTLS_KEY_FILE", keyFile)
	t.Setenv("MTLS_CA_FILE", "")
	if _, enabled, err := ServerTLSOption(); enabled || err != nil {
		t.Fatalf("2/3 vars: want disabled cleanly (documented contract: ALL three enable), got enabled=%v err=%v", enabled, err)
	}
	// All three set but CA unreadable → loud error, never plaintext.
	t.Setenv("MTLS_CA_FILE", filepath.Join(t.TempDir(), "missing.crt"))
	if _, _, err := ServerTLSOption(); err == nil {
		t.Fatal("unreadable CA with mTLS requested must error, not fall back")
	}
	if _, _, err := ClientTLSOption(); err == nil {
		t.Fatal("unreadable CA with mTLS requested must error on the client too")
	}
}
