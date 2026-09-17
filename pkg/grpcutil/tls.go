package grpcutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Mutual TLS for service-to-service gRPC, closing the last open item of the
// distributed-topology work: token auth (AuthInterceptor) already says WHO is
// calling, but the transport itself was plaintext inside the cluster. All
// three settings must be present to enable it; with any unset, servers and
// clients stay plaintext — dev mode, tests, and local runs are unchanged.
//
//	MTLS_CERT_FILE — this service's certificate (PEM)
//	MTLS_KEY_FILE  — its private key (PEM)
//	MTLS_CA_FILE   — the internal CA that signed every service's cert
//
// Certificates are loaded per-handshake via GetCertificate /
// GetClientCertificate rather than once at start: cert-manager renews the
// mounted secret in place, and kubelet updates the files — a callback picks
// the new cert up on the next connection, no pod restart needed.
func mtlsEnv() (certFile, keyFile, caFile string, enabled bool) {
	certFile, keyFile, caFile = os.Getenv("MTLS_CERT_FILE"), os.Getenv("MTLS_KEY_FILE"), os.Getenv("MTLS_CA_FILE")
	return certFile, keyFile, caFile, certFile != "" && keyFile != "" && caFile != ""
}

func caPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mTLS CA %s: no certificates found", caFile)
	}
	return pool, nil
}

// ServerTLSOption returns a grpc.Creds server option requiring a verified
// client certificate, or (nil, false) when mTLS is not configured. Callers
// treat an error as fatal: half-configured mTLS must fail loudly at startup,
// never silently fall back to plaintext.
func ServerTLSOption() (grpc.ServerOption, bool, error) {
	certFile, keyFile, caFile, enabled := mtlsEnv()
	if !enabled {
		return nil, false, nil
	}
	pool, err := caPool(caFile)
	if err != nil {
		return nil, false, err
	}
	// Validate once at startup so a bad mount fails the pod immediately…
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, false, fmt.Errorf("load mTLS keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		// …then reload per handshake so renewals apply without restarts.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return &c, nil
		},
	}
	return grpc.Creds(credentials.NewTLS(cfg)), true, nil
}

// ClientTLSOption returns the dial option matching ServerTLSOption — client
// certificate presented, server verified against the internal CA — or
// (nil, false) when mTLS is not configured.
func ClientTLSOption() (grpc.DialOption, bool, error) {
	certFile, keyFile, caFile, enabled := mtlsEnv()
	if !enabled {
		return nil, false, nil
	}
	pool, err := caPool(caFile)
	if err != nil {
		return nil, false, err
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, false, fmt.Errorf("load mTLS keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return &c, nil
		},
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(cfg)), true, nil
}
