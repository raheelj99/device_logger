// gencerts generates a development PKI: a CA, a server certificate for
// devlogd/licensed, and a client certificate for optional mutual TLS.
// Run: go run ./tools/gencerts [-out-dir deploy/certs] [-hosts extra1,extra2]
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	outDir := flag.String("out-dir", "deploy/certs", "output directory")
	hosts := flag.String("hosts", "", "extra SANs, comma-separated")
	flag.Parse()
	if err := run(*outDir, *hosts); err != nil {
		fmt.Fprintln(os.Stderr, "gencerts:", err)
		os.Exit(1)
	}
}

func run(outDir, extraHosts string) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "devlog dev CA", Organization: []string{"devlog"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writePair(outDir, "ca", caDER, caKey); err != nil {
		return err
	}

	dnsNames := []string{"localhost", "devlogd", "licensed", "host.docker.internal"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, h := range strings.Split(extraHosts, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, h)
		}
	}
	if err := issue(outDir, "server", caCert, caKey, dnsNames, ips,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}); err != nil {
		return err
	}
	if err := issue(outDir, "client", caCert, caKey, nil, nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}); err != nil {
		return err
	}

	fmt.Printf("wrote ca/server/client certificates to %s (SANs: %s)\n",
		outDir, strings.Join(dnsNames, ", "))
	return nil
}

func issue(outDir, name string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey,
	dnsNames []string, ips []net.IP, usage []x509.ExtKeyUsage) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "devlog " + name, Organization: []string{"devlog"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writePair(outDir, name, der, key)
}

func writePair(dir, name string, certDER []byte, key *ecdsa.PrivateKey) error {
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), certPEM, 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return os.WriteFile(filepath.Join(dir, name+".key"), keyPEM, 0o600)
}
