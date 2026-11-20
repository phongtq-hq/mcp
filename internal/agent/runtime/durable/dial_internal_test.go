package durable

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// writeCA ghi một chứng chỉ CA tự ký ra file PEM và trả đường dẫn.
func writeCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDialOptionsFromEnv(t *testing.T) {
	// Ngoài local, kết nối không TLS bị từ chối; ở local thì được.
	if _, err := dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_ADDRESS": "t:7233"}), false); err == nil {
		t.Error("không TLS ngoài local phải bị từ chối")
	}
	o, err := dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_ADDRESS": "t:7233"}), true)
	if err != nil || o.TLS != nil || o.Address != "t:7233" {
		t.Errorf("local không TLS: %+v, %v", o, err)
	}

	// HQ_TEMPORAL_TLS=1: TLS với kho CA của hệ thống (dịch vụ quản lý).
	o, err = dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_TLS": "1", "HQ_TEMPORAL_API_KEY": "k", "HQ_TEMPORAL_NAMESPACE": "ns"}), false)
	if err != nil || o.TLS == nil || o.TLS.RootCAs != nil || o.APIKey != "k" || o.Namespace != "ns" {
		t.Errorf("HQ_TEMPORAL_TLS=1: %+v, %v", o, err)
	}

	// CA riêng tự bật TLS, thay kho CA của hệ thống, kèm tên server để kiểm chứng chỉ.
	ca := writeCA(t)
	o, err = dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_TLS_CA_FILE": ca, "HQ_TEMPORAL_TLS_SERVER_NAME": "temporal"}), false)
	if err != nil || o.TLS == nil || o.TLS.RootCAs == nil || o.TLS.ServerName != "temporal" {
		t.Fatalf("CA riêng: %+v, %v", o, err)
	}
	if o.TLS.InsecureSkipVerify {
		t.Error("không bao giờ tắt kiểm chứng chỉ của server")
	}

	// File CA hỏng hoặc thiếu làm process không lên, không lặng lẽ lùi về kho hệ thống.
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("không phải PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bad, filepath.Join(t.TempDir(), "missing.pem")} {
		if _, err := dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_TLS_CA_FILE": path}), false); err == nil {
			t.Errorf("file CA %q phải bị từ chối", path)
		}
	}
	// Chứng chỉ client thiếu khoá: lỗi, không nối một chiều.
	if _, err := dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_TLS_CERT_FILE": ca}), false); err == nil {
		t.Error("chứng chỉ client không có khoá phải bị từ chối")
	}
	// Tên server mà không có TLS là cấu hình sai, kể cả ở local.
	if _, err := dialOptionsFromEnv(envOf(map[string]string{"HQ_TEMPORAL_TLS_SERVER_NAME": "temporal"}), true); err == nil {
		t.Error("HQ_TEMPORAL_TLS_SERVER_NAME không kèm TLS phải bị từ chối")
	}
}
