package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// GenerateSelfSignedCertKeyPEM generates a self-signed X.509 certificate and a private key,
// returning them as PEM-encoded byte slices.
// The host parameter is used for the certificate's DNS names or IP addresses.
func GenerateSelfSignedCertKeyPEM(hostname string) (certPEMBytes []byte, keyPEMBytes []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	notBefore := time.Now()
	// Valid for 1 day for testing purposes, common for temporary certs
	notAfter := notBefore.Add(24 * time.Hour)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Test Org"}, // Simple organization for test certs
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	// Add the provided hostname and common local addresses to the certificate
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else if hostname != "" {
		template.DNSNames = append(template.DNSNames, hostname)
	}

	// Always include localhost and 127.0.0.1 for convenience in testing
	template.DNSNames = append(template.DNSNames, "localhost")
	template.IPAddresses = append(template.IPAddresses, net.IPv4(127, 0, 0, 1))
	// If IPv6 is available on the system, ::1 might also be useful for some test setups
	// template.IPAddresses = append(template.IPAddresses, net.ParseIP("::1"))

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEMBlock := &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}
	certPEMBytes = pem.EncodeToMemory(certPEMBlock)
	if certPEMBytes == nil {
		return nil, nil, fmt.Errorf("failed to encode certificate to PEM")
	}

	// Marshal the private key into PKCS8 format
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal private key: %w", err)
	}
	keyPEMBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}
	keyPEMBytes = pem.EncodeToMemory(keyPEMBlock)
	if keyPEMBytes == nil {
		return nil, nil, fmt.Errorf("failed to encode private key to PEM")
	}

	return certPEMBytes, keyPEMBytes, nil
}

// GenerateSelfSignedCertKeyFiles generates a self-signed X.509 certificate and private key,
// writes them to temporary files within a directory created by t.TempDir(),
// and returns the paths to these files.
// The host parameter is used for the certificate's DNS names or IP addresses.
func GenerateSelfSignedCertKeyFiles(t *testing.T, host string) (certFilePath string, keyFilePath string, err error) {
	t.Helper()

	certPEMBytes, keyPEMBytes, err := GenerateSelfSignedCertKeyPEM(host)
	if err != nil {
		return "", "", fmt.Errorf("GenerateSelfSignedCertKeyPEM(%s) failed: %w", host, err)
	}

	tempDir := t.TempDir()

	certFilePath = filepath.Join(tempDir, "cert.pem")
	keyFilePath = filepath.Join(tempDir, "key.pem")

	if err := os.WriteFile(certFilePath, certPEMBytes, 0600); err != nil {
		return "", "", fmt.Errorf("failed to write certificate file %s: %w", certFilePath, err)
	}
	if err := os.WriteFile(keyFilePath, keyPEMBytes, 0600); err != nil {
		return "", "", fmt.Errorf("failed to write key file %s: %w", keyFilePath, err)
	}

	return certFilePath, keyFilePath, nil
}
