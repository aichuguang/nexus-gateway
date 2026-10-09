package engine

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"w103d/gateway/internal/model"
)

// ParseCertificate accepts only a complete, currently valid PEM certificate chain
// and one unencrypted private key. Secret material is never returned in errors.
func ParseCertificate(id, name string, certPEM, keyPEM []byte) (model.Certificate, error) {
	var result model.Certificate
	if !identifier.MatchString(id) {
		return result, fmt.Errorf("证书 ID 无效")
	}
	remaining := bytes.TrimSpace(certPEM)
	var chain []*x509.Certificate
	now := time.Now()
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return result, fmt.Errorf("证书文件里含有非证书内容")
		}
		block, rest := pem.Decode(remaining)
		consumed := remaining[:len(remaining)-len(rest)]
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || bytes.Count(consumed, []byte("-----BEGIN ")) != 1 {
			return result, fmt.Errorf("证书 PEM 格式不正确")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return result, fmt.Errorf("证书 DER 内容不正确")
		}
		if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return result, fmt.Errorf("证书已过期或尚未生效")
		}
		chain = append(chain, cert)
		remaining = bytes.TrimSpace(rest)
	}
	if len(chain) == 0 {
		return result, fmt.Errorf("证书文件是空的")
	}
	for i := 0; i+1 < len(chain); i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return result, fmt.Errorf("证书链的顺序或签名不正确")
		}
	}
	remaining = bytes.TrimSpace(keyPEM)
	if !bytes.HasPrefix(remaining, []byte("-----BEGIN ")) {
		return result, fmt.Errorf("私钥 PEM 格式不正确")
	}
	key, rest := pem.Decode(remaining)
	if key == nil || len(key.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || bytes.Count(remaining, []byte("-----BEGIN ")) != 1 {
		return result, fmt.Errorf("私钥文件必须只包含一个未加密的 PEM 私钥")
	}
	switch key.Type {
	case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
	default:
		return result, fmt.Errorf("私钥格式不受支持")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return result, fmt.Errorf("证书与私钥不匹配，或私钥无效")
	}
	leaf := chain[0]
	sum := sha256.Sum256(leaf.Raw)
	domains := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		domains = append(domains, ip.String())
	}
	result = model.Certificate{
		ID: id, Name: name, Domains: domains,
		NotBefore:   leaf.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:    leaf.NotAfter.UTC().Format(time.RFC3339),
		Fingerprint: hex.EncodeToString(sum[:]), Ready: true,
	}
	return result, nil
}

// loadCertificateLeaf re-reads and re-validates the certificate material on disk.
// Host coverage is checked separately against the service's declared domains, so
// this only answers "is this material still exactly what it claims to be".
var materialVersionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// CertificateDirectory accepts legacy slots but never permits a version to escape
// the managed certificate tree. Version directories are immutable once referenced.
func CertificateDirectory(dataDir string, certificate model.Certificate) (string, error) {
	if !identifier.MatchString(certificate.ID) {
		return "", fmt.Errorf("invalid certificate ID")
	}
	dir := filepath.Join(dataDir, "certificates", certificate.ID)
	if certificate.MaterialVersion != "" {
		if !materialVersionPattern.MatchString(certificate.MaterialVersion) {
			return "", fmt.Errorf("invalid certificate material version")
		}
		dir = filepath.Join(dir, "versions", certificate.MaterialVersion)
	}
	return dir, nil
}

func (e *Engine) loadCertificateLeaf(certificate model.Certificate) (model.Certificate, *x509.Certificate, error) {
	certificateID := certificate.ID
	if !identifier.MatchString(certificateID) {
		return model.Certificate{}, nil, fmt.Errorf("证书 ID 无效")
	}
	dir, err := CertificateDirectory(e.DataDir, certificate)
	if err != nil {
		return model.Certificate{}, nil, err
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, "fullchain.pem"))
	if err != nil {
		return model.Certificate{}, nil, fmt.Errorf("证书文件不可读")
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "privkey.pem"))
	if err != nil {
		return model.Certificate{}, nil, fmt.Errorf("私钥文件不可读")
	}
	info, err := ParseCertificate(certificateID, "", certPEM, keyPEM)
	if err != nil {
		return info, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return info, nil, fmt.Errorf("证书 PEM 格式不正确")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return info, nil, fmt.Errorf("证书 DER 内容不正确")
	}
	return info, leaf, nil
}
