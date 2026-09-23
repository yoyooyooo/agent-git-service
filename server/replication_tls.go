package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
)

func replicationTLS(cfg ReplicationFileConfig, resolve func(string) (string, error)) (*tls.Config, error) {
	read := func(value string, private bool) ([]byte, error) {
		path, err := resolve(value)
		if err != nil {
			return nil, err
		}
		return replicationFile(path, private, 1<<20)
	}
	caPEM, err := read(cfg.CAFile, false)
	if err != nil {
		return nil, errors.New("cannot load replication trust file")
	}
	certPEM, err := read(cfg.CertificateFile, false)
	if err != nil {
		return nil, errors.New("cannot load replication server certificate")
	}
	keyPEM, err := read(cfg.PrivateKeyFile, true)
	if err != nil {
		return nil, errors.New("replication private key must be owner-only")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid replication trust")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, errors.New("invalid replication certificate/key pair")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid replication leaf certificate")
	}
	intermediates := x509.NewCertPool()
	for _, der := range certificate.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, errors.New("invalid replication certificate chain")
		}
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: cfg.ServerName, Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, errors.New("replication server certificate fails name, validity or trust checks")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
		// Trust is loaded once per owning process. A restart must perform a
		// fresh handshake against the newly loaded certificate configuration.
		SessionTicketsDisabled: true, NextProtos: []string{"http/1.1"},
	}, nil
}
