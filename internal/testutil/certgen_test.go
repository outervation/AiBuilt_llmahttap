package testutil_test

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"example.com/llmahttap/v2/internal/testutil"
)

func TestGenerateSelfSignedCertKeyPEM(t *testing.T) {
	hostnames := []string{"localhost", "127.0.0.1", "example.com"}

	for _, host := range hostnames {
		t.Run(host, func(t *testing.T) {
			_ = filepath.Separator // Use filepath to satisfy import
			var _ reflect.Type     // Use reflect to satisfy import
			certPEM, keyPEM, err := testutil.GenerateSelfSignedCertKeyPEM(host)
			if err != nil {
				t.Fatalf("GenerateSelfSignedCertKeyPEM(%s) failed: %v", host, err)
			}

			if len(certPEM) == 0 {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) returned empty certificate PEM", host)
			}
			if len(keyPEM) == 0 {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) returned empty key PEM", host)
			}

			// Check cert
			certBlock, rest := pem.Decode(certPEM)
			if certBlock == nil {
				t.Fatalf("GenerateSelfSignedCertKeyPEM(%s) failed to decode cert PEM", host)
			}
			if len(rest) > 0 {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) returned cert PEM with trailing data", host)
			}
			if certBlock.Type != "CERTIFICATE" {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) cert PEM block type is %s, want CERTIFICATE", host, certBlock.Type)
			}
			cert, err := x509.ParseCertificate(certBlock.Bytes)
			if err != nil {
				t.Fatalf("GenerateSelfSignedCertKeyPEM(%s) failed to parse certificate: %v", host, err)
			}

			// Check key
			keyBlock, rest := pem.Decode(keyPEM)
			if keyBlock == nil {
				t.Fatalf("GenerateSelfSignedCertKeyPEM(%s) failed to decode key PEM", host)
			}
			if len(rest) > 0 {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) returned key PEM with trailing data", host)
			}
			if keyBlock.Type != "PRIVATE KEY" { // As per x509.MarshalPKCS8PrivateKey
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) key PEM block type is %s, want PRIVATE KEY", host, keyBlock.Type)
			}
			key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
			if err != nil {
				t.Fatalf("GenerateSelfSignedCertKeyPEM(%s) failed to parse private key: %v", host, err)
			}
			if _, ok := key.(*ecdsa.PrivateKey); !ok {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) parsed private key is not an *ecdsa.PrivateKey, got %T", host, key)
			}

			// Check if the cert and key can be loaded as a pair using tls.X509KeyPair
			_, err = tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) tls.X509KeyPair(certPEM, keyPEM) failed: %v", host, err)
			}

			// Verify that the public key in the certificate matches the public key of the private key
			if certPubKey, ok := cert.PublicKey.(*ecdsa.PublicKey); ok {
				if privKey, ok := key.(*ecdsa.PrivateKey); ok {
					if !privKey.PublicKey.Equal(certPubKey) {
						t.Errorf("GenerateSelfSignedCertKeyPEM(%s) public key in certificate does not match public key of private key", host)
					}
				} else {
					// Already handled by key type check below, but defensive
					t.Errorf("GenerateSelfSignedCertKeyPEM(%s) parsed private key is not an *ecdsa.PrivateKey, got %T", host, key)
				}
			} else {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) public key in certificate is not an *ecdsa.PublicKey, got %T", host, cert.PublicKey)
			}

			// Verify hostname in certificate
			foundHost := false
			if parsedIP := net.ParseIP(host); parsedIP != nil {
				for _, ip := range cert.IPAddresses {
					if ip.Equal(parsedIP) {
						foundHost = true
						break
					}
				}
			} else {
				for _, dnsName := range cert.DNSNames {
					if dnsName == host {
						foundHost = true
						break
					}
				}
			}
			if !foundHost && host != "" { // host can be empty string for default localhost/127.0.0.1
				// Check default localhost if host was an IP that might not be directly in DNSNames
				if net.ParseIP(host) != nil {
					isLocalhost := false
					for _, dnsName := range cert.DNSNames {
						if dnsName == "localhost" {
							isLocalhost = true
							break
						}
					}
					if !isLocalhost {
						t.Errorf("GenerateSelfSignedCertKeyPEM(%s) did not find '%s' or 'localhost' in cert DNSNames/IPAddresses: DNSNames=%v, IPAddresses=%v", host, host, cert.DNSNames, cert.IPAddresses)
					}
				} else {
					t.Errorf("GenerateSelfSignedCertKeyPEM(%s) did not find '%s' in cert DNSNames/IPAddresses: DNSNames=%v, IPAddresses=%v", host, host, cert.DNSNames, cert.IPAddresses)
				}
			}

			// Check if localhost and 127.0.0.1 are always present as per certgen.go
			foundLocalhostDNS := false
			for _, dnsName := range cert.DNSNames {
				if dnsName == "localhost" {
					foundLocalhostDNS = true
					break
				}
			}
			if !foundLocalhostDNS {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) cert missing 'localhost' in DNSNames: %v", host, cert.DNSNames)
			}

			found127001IP := false
			for _, ip := range cert.IPAddresses {
				if ip.Equal(net.IPv4(127, 0, 0, 1)) {
					found127001IP = true
					break
				}
			}
			if !found127001IP {
				t.Errorf("GenerateSelfSignedCertKeyPEM(%s) cert missing '127.0.0.1' in IPAddresses: %v", host, cert.IPAddresses)
			}
		})
	}
}

func TestGenerateSelfSignedCertKeyFiles(t *testing.T) {
	host := "test.example.com"
	certFilePath, keyFilePath, err := testutil.GenerateSelfSignedCertKeyFiles(t, host)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCertKeyFiles(%s) failed: %v", host, err)
	}

	if certFilePath == "" {
		t.Error("GenerateSelfSignedCertKeyFiles returned empty certFilePath")
	}
	if keyFilePath == "" {
		t.Error("GenerateSelfSignedCertKeyFiles returned empty keyFilePath")
	}

	// Check files exist
	if _, err := os.Stat(certFilePath); os.IsNotExist(err) {
		t.Errorf("Certificate file %s does not exist", certFilePath)
	}
	if _, err := os.Stat(keyFilePath); os.IsNotExist(err) {
		t.Errorf("Key file %s does not exist", keyFilePath)
	}

	// Read and verify content (basic check, more thorough checks in TestGenerateSelfSignedCertKeyPEM)
	certPEM, err := os.ReadFile(certFilePath)
	if err != nil {
		t.Fatalf("Failed to read cert file %s: %v", certFilePath, err)
	}
	keyPEM, err := os.ReadFile(keyFilePath)
	if err != nil {
		t.Fatalf("Failed to read key file %s: %v", keyFilePath, err)
	}

	if len(certPEM) == 0 {
		t.Errorf("Cert file %s is empty", certFilePath)
	}
	if len(keyPEM) == 0 {
		t.Errorf("Key file %s is empty", keyFilePath)
	}

	// Try loading the pair
	_, err = tls.LoadX509KeyPair(certFilePath, keyFilePath)
	if err != nil {
		t.Errorf("tls.LoadX509KeyPair(%s, %s) failed: %v", certFilePath, keyFilePath, err)
	}

	// Verify cert content briefly for the given host
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatalf("Failed to decode cert PEM from file %s", certFilePath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse certificate from file %s: %v", certFilePath, err)
	}

	foundHost := false

	// Verify key content from file
	keyBlock, restKey := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatalf("Failed to decode key PEM from file %s", keyFilePath)
	}
	if len(restKey) > 0 {
		t.Errorf("Key PEM from file %s has trailing data", keyFilePath)
	}
	if keyBlock.Type != "PRIVATE KEY" {
		t.Errorf("Key PEM block type from file %s is %s, want PRIVATE KEY", keyFilePath, keyBlock.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse private key from file %s: %v", keyFilePath, err)
	}
	if _, ok := key.(*ecdsa.PrivateKey); !ok {
		t.Errorf("Parsed private key from file %s is not an *ecdsa.PrivateKey, got %T", keyFilePath, key)
	}
	for _, dnsName := range cert.DNSNames {
		if dnsName == host {
			foundHost = true
			break
		}
	}
	if !foundHost {
		t.Errorf("GenerateSelfSignedCertKeyFiles(%s) did not find '%s' in cert DNSNames: %v", host, host, cert.DNSNames)
	}

	// Check for temp dir cleanup (implicitly done by t.TempDir())
	// To explicitly check, we could try to stat the parent of certFilePath after the test,
	// but t.TempDir()'s cleanup is generally reliable.
	// For example, one could store tempDir := filepath.Dir(certFilePath)
	// and then add a t.Cleanup(func() { if _, err := os.Stat(tempDir); !os.IsNotExist(err) { t.Errorf("Temp dir %s not cleaned up", tempDir)}})
	// However, this might be flaky if t.TempDir() has specific cleanup timing.
	// Relying on t.TempDir behavior is usually sufficient.

	// Test with empty host (should still generate valid certs for localhost)
	t.Run("EmptyHost", func(t *testing.T) {
		certFilePathEmpty, keyFilePathEmpty, errEmpty := testutil.GenerateSelfSignedCertKeyFiles(t, "")
		if errEmpty != nil {
			t.Fatalf("GenerateSelfSignedCertKeyFiles(\"\") failed: %v", errEmpty)
		}
		_, errLoad := tls.LoadX509KeyPair(certFilePathEmpty, keyFilePathEmpty)
		if errLoad != nil {
			t.Errorf("tls.LoadX509KeyPair for empty host generated files failed: %v", errLoad)
		}
	})
}
