package driver_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

type testPKI struct {
	caPath     string
	caPool     *x509.CertPool
	serverCert tls.Certificate
	clientCert string
	clientKey  string
}

func generateTestPKI(t *testing.T, serverDNS string, serverIP net.IP) testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "SQLM Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write ca pem: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	// Server cert
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen server key: %v", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverDNS},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{serverDNS},
	}
	if serverIP != nil {
		srvTmpl.IPAddresses = []net.IP{serverIP}
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caTmpl, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER})
	serverCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}

	// Client cert (mTLS)
	cliKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen client key: %v", err)
	}
	cliTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "sqlm_mtls"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	cliDER, err := x509.CreateCertificate(rand.Reader, cliTmpl, caTmpl, &cliKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	cliCertPath := filepath.Join(dir, "client.pem")
	cliKeyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(cliCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER}), 0o600); err != nil {
		t.Fatalf("write client cert: %v", err)
	}
	cliKeyDER, err := x509.MarshalECPrivateKey(cliKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	if err := os.WriteFile(cliKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: cliKeyDER}), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	return testPKI{
		caPath:     caPath,
		caPool:     caPool,
		serverCert: serverCert,
		clientCert: cliCertPath,
		clientKey:  cliKeyPath,
	}
}

func runTLSHandshake(t *testing.T, serverCfg, clientCfg *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	srvStateCh := make(chan tls.ConnectionState, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		_ = tc.Handshake()
		srvStateCh <- tc.ConnectionState()
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return <-srvStateCh, nil
}

func TestValidateTLSConfigMatrix(t *testing.T) {
	validCases := []dto.TLSConfig{
		{Mode: ""},
		{Mode: "disable"},
		{Mode: "prefer"},
		{Mode: "prefer", CertPath: "/a.pem", KeyPath: "/a.key"},
		{Mode: "require"},
		{Mode: "require", CertPath: "/a.pem", KeyPath: "/a.key"},
		{Mode: "verify-ca"},
		{Mode: "verify-ca", CACertPath: "/ca.pem", CertPath: "/a.pem", KeyPath: "/a.key"},
		{Mode: "verify-full", CACertPath: "/ca.pem", CertPath: "/a.pem", KeyPath: "/a.key"},
	}
	for _, tc := range validCases {
		if err := driver.ValidateTLSConfig(tc); err != nil {
			t.Errorf("expected valid TLSConfig %+v, got error: %v", tc, err)
		}
	}

	invalidCases := []struct {
		name string
		cfg  dto.TLSConfig
	}{
		{"unknown mode", dto.TLSConfig{Mode: "strict"}},
		{"half keypair cert only", dto.TLSConfig{Mode: "require", CertPath: "/a.pem"}},
		{"half keypair key only", dto.TLSConfig{Mode: "verify-full", KeyPath: "/a.key"}},
		{"disable with ca", dto.TLSConfig{Mode: "disable", CACertPath: "/ca.pem"}},
		{"disable with client cert", dto.TLSConfig{Mode: "disable", CertPath: "/a.pem", KeyPath: "/a.key"}},
		{"prefer with ca", dto.TLSConfig{Mode: "prefer", CACertPath: "/ca.pem"}},
		{"require with ca", dto.TLSConfig{Mode: "require", CACertPath: "/ca.pem"}},
	}
	for _, tc := range invalidCases {
		err := driver.ValidateTLSConfig(tc.cfg)
		if err == nil {
			t.Errorf("case %q: expected error for %+v, got nil", tc.name, tc.cfg)
			continue
		}
		if err.Code != dto.ErrInvalidArgument {
			t.Errorf("case %q: expected ErrInvalidArgument, got %s", tc.name, err.Code)
		}
	}
}

func TestBuildTLSConfigVerifyCAAndVerifyFullAndMTLS(t *testing.T) {
	trustedPKI := generateTestPKI(t, "db.internal", net.ParseIP("127.0.0.1"))
	untrustedPKI := generateTestPKI(t, "db.internal", net.ParseIP("127.0.0.1"))

	// 1. verify-ca: accepts trusted CA even when serverName does not match SAN.
	verifyCACfg, err := driver.BuildTLSConfig(dto.TLSConfig{
		Mode:       "verify-ca",
		CACertPath: trustedPKI.caPath,
	}, "wrong-host.example")
	if err != nil {
		t.Fatalf("BuildTLSConfig verify-ca: %v", err)
	}
	// Clone() mimics go-sql-driver/mysql Config.Clone()
	if _, err := runTLSHandshake(t, &tls.Config{Certificates: []tls.Certificate{trustedPKI.serverCert}}, verifyCACfg.Clone()); err != nil {
		t.Fatalf("verify-ca should accept trusted CA with mismatched hostname, got: %v", err)
	}

	// 2. verify-ca: MUST reject certificate signed by untrusted CA (fixes historical bug!).
	if _, err := runTLSHandshake(t, &tls.Config{Certificates: []tls.Certificate{untrustedPKI.serverCert}}, verifyCACfg.Clone()); err == nil {
		t.Fatalf("verify-ca MUST reject untrusted CA certificate")
	}

	// 3. verify-full: accepts matching SAN, rejects mismatched hostname.
	verifyFullOK, err := driver.BuildTLSConfig(dto.TLSConfig{
		Mode:       "verify-full",
		CACertPath: trustedPKI.caPath,
	}, "db.internal")
	if err != nil {
		t.Fatalf("BuildTLSConfig verify-full ok: %v", err)
	}
	if _, err := runTLSHandshake(t, &tls.Config{Certificates: []tls.Certificate{trustedPKI.serverCert}}, verifyFullOK.Clone()); err != nil {
		t.Fatalf("verify-full should accept matching SAN, got: %v", err)
	}

	verifyFullBad, err := driver.BuildTLSConfig(dto.TLSConfig{
		Mode:       "verify-full",
		CACertPath: trustedPKI.caPath,
	}, "wrong-host.example")
	if err != nil {
		t.Fatalf("BuildTLSConfig verify-full bad: %v", err)
	}
	if _, err := runTLSHandshake(t, &tls.Config{Certificates: []tls.Certificate{trustedPKI.serverCert}}, verifyFullBad.Clone()); err == nil {
		t.Fatalf("verify-full MUST reject mismatched ServerName")
	}

	// 4. mTLS works on prefer, require, verify-ca, and verify-full.
	for _, mode := range []string{"prefer", "require", "verify-ca", "verify-full"} {
		tlsCfg := dto.TLSConfig{
			Mode:     mode,
			CertPath: trustedPKI.clientCert,
			KeyPath:  trustedPKI.clientKey,
		}
		if mode == "verify-ca" || mode == "verify-full" {
			tlsCfg.CACertPath = trustedPKI.caPath
		}
		cliCfg, err := driver.BuildTLSConfig(tlsCfg, "db.internal")
		if err != nil {
			t.Fatalf("BuildTLSConfig %s mTLS: %v", mode, err)
		}
		srvCfg := &tls.Config{
			Certificates: []tls.Certificate{trustedPKI.serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    trustedPKI.caPool,
		}
		srvState, err := runTLSHandshake(t, srvCfg, cliCfg.Clone())
		if err != nil {
			t.Fatalf("mTLS handshake failed for mode=%s: %v", mode, err)
		}
		if len(srvState.PeerCertificates) == 0 || srvState.PeerCertificates[0].Subject.CommonName != "sqlm_mtls" {
			t.Fatalf("mode=%s expected server to receive client cert sqlm_mtls, got %+v", mode, srvState.PeerCertificates)
		}
	}
}
