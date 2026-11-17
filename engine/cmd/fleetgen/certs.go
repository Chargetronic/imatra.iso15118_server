// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Client certificates for the virtual fleet (13.11.2026, OSS scope, A3): the
// deployment admits devices by certificate only (CN = device id). fleetgen signs
// one per device itself, in memory, with a test CA given on the command line
// (-client-ca, -client-ca-key): the deployment's own CA in PKI_MODE=own, or the
// test copy of the service layer's cloud root for a load test with the layer.
// Never point it at a production CA key.

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
	"os"
	"time"
)

type fleetCA struct {
	cert *x509.Certificate
	key  any
	days int
}

func loadFleetCA(certFile, keyFile string, days int) (*fleetCA, error) {
	cb, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	kb, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	cp, _ := pem.Decode(cb)
	kp, _ := pem.Decode(kb)
	if cp == nil || kp == nil {
		return nil, errors.New("client CA: certificate or key is not PEM")
	}
	cert, err := x509.ParseCertificate(cp.Bytes)
	if err != nil {
		return nil, err
	}
	var key any
	switch kp.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kp.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(kp.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("client CA key: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("client CA: the certificate is not a CA")
	}
	return &fleetCA{cert: cert, key: key, days: days}, nil
}

// issue: a new P-256 key and a client certificate with CN = id.
func (c *fleetCA) issue(id string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: id},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, c.days),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
