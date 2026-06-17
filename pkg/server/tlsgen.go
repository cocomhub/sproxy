// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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
	"time"
)

// GenerateSelfSignedCert generates an ECDSA P-256 self-signed certificate and
// writes it as PEM-encoded cert and key files. If the parent directory does not
// exist it is created automatically.
func GenerateSelfSignedCert(certFile, keyFile string) error {
	// Generate ECDSA P-256 private key
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ECDSA key: %w", err)
	}

	// Generate a random serial number
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "sproxy.local",
			Organization: []string{"Cocomhub"},
		},
		NotBefore: now,
		NotAfter:  now.Add(10 * 365 * 24 * time.Hour), // 10 years

		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		DNSNames:    []string{"localhost", "sproxy.local"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	// Self-sign
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	// Ensure parent directory exists
	dir := filepath.Dir(certFile)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create cert directory %s: %w", dir, err)
	}

	// Write cert PEM
	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("create cert file %s: %w", certFile, err)
	}
	defer certOut.Close()

	if err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return fmt.Errorf("encode cert PEM: %w", err)
	}

	// Write key PEM
	keyOut, err := os.Create(keyFile)
	if err != nil {
		return fmt.Errorf("create key file %s: %w", keyFile, err)
	}
	defer keyOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal EC private key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("encode key PEM: %w", err)
	}

	return nil
}
