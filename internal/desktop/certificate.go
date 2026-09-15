package desktop

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
	"os"
	"path/filepath"
	"time"
)

// ensureCertificate сохраняет самоподписанный сертификат конкретного host.
// Это конечный сертификат, не CA: его ключ не может подписывать другие сайты.
// Явное локальное доверие ограничено ssl/имя host. Срок — 365 дней:
// длинный срок самоподписанного leaf отклоняется SSL-политикой macOS.
// Повторная установка сохраняет ключ и доверие. Повреждённые файлы не заменяются
// молча; при истечении срока создаётся новая пара и Install повторяет доверие.
func ensureCertificate(p Paths, now time.Time) error {
	certPath, keyPath := filepath.Join(p.Dir, "server.pem"), filepath.Join(p.Dir, "server.key")
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil && keyErr == nil {
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return err
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		if err = cert.VerifyHostname(Host); err != nil {
			return err
		}
		if now.Before(cert.NotBefore) {
			return errors.New("сертификат Lawa ещё не действует; проверьте часы")
		}
		if now.Add(30 * 24 * time.Hour).Before(cert.NotAfter) {
			return nil
		}
	} else if !os.IsNotExist(certErr) || !os.IsNotExist(keyErr) {
		return errors.New("неполная или недоступная пара server.pem/server.key; восстановите файлы из резервной копии либо переместите оба файла и повторите desktop-install")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: Host}, DNSNames: []string{Host}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// Между двумя rename возможен сбой питания. Следующий вызов явно обнаружит
	// неполную пару вместо использования нового ключа со старым сертификатом.
	if err = writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return err
	}
	return writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
}
