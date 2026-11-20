package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Chứng chỉ nội bộ cho Temporal tự vận hành (ADR-0017). Ngoài local, platform chỉ nối tới
// Temporal qua TLS, và ai ghi được vào Temporal thì điều khiển được worker: frontend của
// Temporal vì thế đòi chứng chỉ client (mTLS), và chỉ `api` + `worker` được cấp.
//
// Bố cục thư mục (một volume, mount chỉ đọc vào các container):
//
//	ca.pem            chứng chỉ CA (công khai)
//	ca.key            khoá CA, chỉ root đọc được: chỉ lệnh này dùng để cấp lại chứng chỉ lá
//	server/cert.pem   chứng chỉ của Temporal (frontend + internode)   khoá: server/key.pem (uid của Temporal)
//	client/cert.pem   chứng chỉ client của api / worker               khoá: client/key.pem (uid của platform)
const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 5 * 365 * 24 * time.Hour
	maxCertFile  = 1 << 20
)

type certsConfig struct {
	Dir         string
	ServerName  string
	ServerUID   int
	ClientUID   int
	RenewBefore time.Duration
	Now         time.Time
}

func temporalCertsInit(args []string) error {
	fs := flag.NewFlagSet("temporal-certs init", flag.ContinueOnError)
	cfg := certsConfig{Now: time.Now()}
	fs.StringVar(&cfg.Dir, "dir", "", "thư mục chứa chứng chỉ (volume dùng chung với Temporal, api, worker)")
	fs.StringVar(&cfg.ServerName, "server-name", "temporal", "tên DNS của Temporal trong mạng compose (SAN của chứng chỉ server)")
	fs.IntVar(&cfg.ServerUID, "server-uid", 1000, "uid của process Temporal (chủ sở hữu khoá server); -1 = không chown")
	fs.IntVar(&cfg.ClientUID, "client-uid", 65532, "uid của process api / worker (chủ sở hữu khoá client); -1 = không chown")
	fs.DurationVar(&cfg.RenewBefore, "renew-before", 365*24*time.Hour, "cấp lại chứng chỉ lá khi thời hạn còn lại ngắn hơn chừng này")
	if err := fs.Parse(args); err != nil {
		return err
	}
	changed, err := ensureCerts(cfg)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Println("chứng chỉ Temporal: còn hạn, không đổi gì")
		return nil
	}
	for _, c := range changed {
		fmt.Println("chứng chỉ Temporal: đã cấp", c)
	}
	return nil
}

// ensureCerts tạo CA và hai chứng chỉ lá nếu chưa có, cấp lại chứng chỉ lá sắp hết hạn
// hoặc không do CA hiện tại ký. Chạy lại được. Trả về danh sách thứ đã cấp mới.
func ensureCerts(cfg certsConfig) ([]string, error) {
	if cfg.Dir == "" || cfg.ServerName == "" {
		return nil, errors.New("cần -dir và -server-name")
	}
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, "server"), filepath.Join(cfg.Dir, "client")} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // thư mục chỉ chứa chứng chỉ công khai và khoá 0400
			return nil, err
		}
	}
	var changed []string
	caCertPath, caKeyPath := filepath.Join(cfg.Dir, "ca.pem"), filepath.Join(cfg.Dir, "ca.key")
	ca, caKey, err := loadPair(caCertPath, caKeyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if ca, caKey, err = newCA(cfg.Now); err != nil {
			return nil, err
		}
		if err := writePair(caCertPath, caKeyPath, ca.Raw, caKey, -1); err != nil {
			return nil, err
		}
		changed = append(changed, "CA")
	case err != nil:
		return nil, fmt.Errorf("CA hiện có: %w", err)
	case cfg.Now.After(ca.NotAfter.Add(-cfg.RenewBefore)):
		// Đổi CA làm mọi bên phải khởi động lại cùng lúc: không tự làm, để người vận hành chủ động.
		return nil, fmt.Errorf("CA hết hạn ngày %s: xoá volume chứng chỉ rồi triển khai lại để cấp CA mới", ca.NotAfter.Format(time.DateOnly))
	}

	leaves := []struct {
		name, dir string
		uid       int
		tmpl      *x509.Certificate
	}{
		// Temporal dùng chứng chỉ server cả ở vai client (system worker của nó gọi frontend).
		{"server", "server", cfg.ServerUID, &x509.Certificate{
			Subject: pkix.Name{CommonName: cfg.ServerName}, DNSNames: []string{cfg.ServerName, "localhost"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}},
		{"client", "client", cfg.ClientUID, &x509.Certificate{
			Subject: pkix.Name{CommonName: "hq-platform"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}},
	}
	for _, l := range leaves {
		certPath, keyPath := filepath.Join(cfg.Dir, l.dir, "cert.pem"), filepath.Join(cfg.Dir, l.dir, "key.pem")
		cur, _, err := loadPair(certPath, keyPath)
		if err == nil && cur.CheckSignatureFrom(ca) == nil && cfg.Now.Before(cur.NotAfter.Add(-cfg.RenewBefore)) &&
			(l.name != "server" || cur.VerifyHostname(cfg.ServerName) == nil) {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("chứng chỉ %s hiện có: %w", l.name, err)
		}
		der, key, err := newLeaf(l.tmpl, ca, caKey, cfg.Now)
		if err != nil {
			return nil, err
		}
		if err := writePair(certPath, keyPath, der, key, l.uid); err != nil {
			return nil, err
		}
		changed = append(changed, l.name)
	}
	return changed, nil
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func newCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: "HQ Temporal internal CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(caValidity),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func newLeaf(tmpl, ca *x509.Certificate, caKey *ecdsa.PrivateKey, now time.Time) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	t := *tmpl
	t.SerialNumber, t.NotBefore, t.NotAfter = sn, now.Add(-time.Hour), now.Add(leafValidity)
	if t.NotAfter.After(ca.NotAfter) {
		t.NotAfter = ca.NotAfter
	}
	t.KeyUsage, t.BasicConstraintsValid = x509.KeyUsageDigitalSignature, true
	der, err := x509.CreateCertificate(rand.Reader, &t, ca, &key.PublicKey, caKey)
	return der, key, err
}

// loadPair đọc một chứng chỉ và khoá EC của nó. Thiếu một trong hai file là os.ErrNotExist.
func loadPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := readSmall(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := readSmall(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("file không phải PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("khoá không khớp chứng chỉ")
	}
	return cert, key, nil
}

func readSmall(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxCertFile {
		return nil, fmt.Errorf("%s: file quá lớn", path)
	}
	return os.ReadFile(path) //nolint:gosec // đường dẫn do người vận hành cấu hình
}

// writePair ghi chứng chỉ (0644) và khoá (0400, chủ sở hữu uid nếu uid >= 0). Khoá được
// ghi ra file tạm rồi đổi tên, để một process đang đọc không thấy file dở dang.
func writePair(certPath, keyPath string, certDER []byte, key *ecdsa.PrivateKey, uid int) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	write := func(path string, block *pem.Block, mode os.FileMode, owner int) error {
		tmp := path + ".tmp"
		_ = os.Remove(tmp)
		if err := os.WriteFile(tmp, pem.EncodeToMemory(block), mode); err != nil {
			return err
		}
		if owner >= 0 {
			if err := os.Chown(tmp, owner, -1); err != nil {
				return fmt.Errorf("chown %s cho uid %d (lệnh này cần chạy bằng root): %w", path, owner, err)
			}
		}
		return os.Rename(tmp, path)
	}
	if err := write(keyPath, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}, 0o400, uid); err != nil {
		return err
	}
	return write(certPath, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}, 0o644, -1)
}
