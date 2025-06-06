package testutil

import (
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
)

// GenerateSelfSignedCertKeyPEM generates a self-signed X.509 certificate and a private key,
// returning them as PEM-encoded byte slices.
// The host parameter is used for the certificate's DNS names or IP addresses.
func GenerateSelfSignedCertKeyPEM(hostname string) (certPEMBytes []byte, keyPEMBytes []byte, err error) {
	// Generate a private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	// Create a certificate template
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Test Co"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// Always add localhost and 127.0.0.1 for test convenience
	template.DNSNames = []string{"localhost"}
	template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}

	if ip := net.ParseIP(hostname); ip != nil {
		// It's an IP address, add it if it's not 127.0.0.1 (which is already there)
		if !ip.Equal(net.ParseIP("127.0.0.1")) {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	} else {
		// It's a DNS name, add it if it's not "localhost" (which is already there)
		if hostname != "localhost" && hostname != "" {
			template.DNSNames = append(template.DNSNames, hostname)
		}
	}

	// Create the certificate
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}

	// PEM-encode the certificate
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	// PEM-encode the private key
	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return certPEM, keyPEM, nil
}

// GenerateSelfSignedCertKeyFiles generates a self-signed X.509 certificate and private key,
// writes them to temporary files within a directory created by t.TempDir(),
// and returns the paths to these files.
// The host parameter is used for the certificate's DNS names or IP addresses.
func GenerateSelfSignedCertKeyFiles(t *testing.T, host string) (certFilePath string, keyFilePath string, err error) {
	t.Helper()
	certPEM, keyPEM, err := GenerateSelfSignedCertKeyPEM(host)
	if err != nil {
		return "", "", err
	}

	dir := t.TempDir()

	certFilePath = filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certFilePath, certPEM, 0600); err != nil {
		return "", "", err
	}

	keyFilePath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyFilePath, keyPEM, 0600); err != nil {
		return "", "", err
	}

	return certFilePath, keyFilePath, nil
}
