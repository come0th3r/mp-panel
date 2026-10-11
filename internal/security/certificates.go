package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
}

func serial() *big.Int {
	n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		panic(e)
	}
	return n
}
func keyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	b, e := x509.MarshalPKCS8PrivateKey(k)
	if e != nil {
		return nil, e
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), nil
}
func OpenCA(dir string) (*CA, error) {
	d := filepath.Join(dir, "keys")
	if e := os.MkdirAll(d, 0700); e != nil {
		return nil, e
	}
	lock := filepath.Join(d, "ca.lock")
	for i := 0; i < 100; i++ {
		if e := os.Mkdir(lock, 0700); e == nil {
			defer os.Remove(lock)
			return openCALocked(d)
		} else if !os.IsExist(e) {
			return nil, e
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, errors.New("CA initialization lock is busy; inspect data/keys/ca.lock if a previous process crashed")
}
func openCALocked(dir string) (*CA, error) {
	cp := filepath.Join(dir, "ca.crt")
	kp := filepath.Join(dir, "ca.key")
	certPEM, ce := os.ReadFile(cp)
	keyBytes, ke := os.ReadFile(kp)
	if ce == nil && ke == nil {
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyBytes)
		if cb == nil || kb == nil {
			return nil, errors.New("invalid CA PEM")
		}
		cert, e := x509.ParseCertificate(cb.Bytes)
		if e != nil {
			return nil, e
		}
		key, e := x509.ParsePKCS8PrivateKey(kb.Bytes)
		if e != nil {
			return nil, e
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("invalid CA key")
		}
		return &CA{Cert: cert, Key: ec, PEM: certPEM}, nil
	}
	if !os.IsNotExist(ce) || !os.IsNotExist(ke) {
		return nil, errors.New("CA files are missing or unreadable; restore both ca.crt and ca.key")
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, e
	}
	template := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "MP internal agent CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		return nil, e
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, e = keyPEM(key)
	if e != nil {
		return nil, e
	}
	if e = os.WriteFile(kp, keyBytes, 0600); e != nil {
		return nil, e
	}
	if e = os.WriteFile(cp, certPEM, 0600); e != nil {
		return nil, e
	}
	cert, e := x509.ParseCertificate(der)
	return &CA{Cert: cert, Key: key, PEM: certPEM}, e
}
func (c *CA) Issue(name string, server bool) ([]byte, []byte, error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, nil, e
	}
	t := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(2, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		if ip := net.ParseIP(name); ip != nil {
			t.IPAddresses = []net.IP{ip}
		} else {
			t.DNSNames = []string{name}
		}
	} else {
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		u, _ := url.Parse("urn:mp:node:" + name)
		t.URIs = []*url.URL{u}
	}
	der, e := x509.CreateCertificate(rand.Reader, t, c.Cert, &key.PublicKey, c.Key)
	if e != nil {
		return nil, nil, e
	}
	kp, e := keyPEM(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), kp, e
}
func (c *CA) TLSConfig(host string) (*tls.Config, error) {
	cp, kp, e := c.Issue(host, true)
	if e != nil {
		return nil, e
	}
	pair, e := tls.X509KeyPair(cp, kp)
	if e != nil {
		return nil, e
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(c.PEM)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}, nil
}
