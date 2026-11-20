package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureCerts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cfg := certsConfig{Dir: dir, ServerName: "temporal", ServerUID: -1, ClientUID: -1, RenewBefore: 365 * 24 * time.Hour, Now: now}

	changed, err := ensureCerts(cfg)
	if err != nil || len(changed) != 3 {
		t.Fatalf("lần đầu: %v, %v (muốn CA + server + client)", changed, err)
	}
	for _, k := range []string{"ca.key", "server/key.pem", "client/key.pem"} {
		info, err := os.Stat(filepath.Join(dir, k))
		if err != nil || info.Mode().Perm() != 0o400 {
			t.Errorf("%s: mode %v, %v; khoá phải là 0400", k, info.Mode().Perm(), err)
		}
	}

	// Chứng chỉ dùng được cho một bắt tay mTLS thật: server kiểm client, client kiểm tên server.
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem không đọc được")
	}
	server, err := tls.LoadX509KeyPair(filepath.Join(dir, "server/cert.pem"), filepath.Join(dir, "server/key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := tls.LoadX509KeyPair(filepath.Join(dir, "client/cert.pem"), filepath.Join(dir, "client/key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	handshake := func(clientCfg *tls.Config) error {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
			Certificates: []tls.Certificate{server}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		done := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = c.Close() }()
			if err := c.(*tls.Conn).Handshake(); err != nil {
				done <- err
				return
			}
			_, err = c.Write([]byte("k"))
			done <- err
		}()
		c, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		// TLS 1.3: server từ chối chứng chỉ client sau khi Dial đã trả về; chỉ lần đọc này cho biết.
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err != nil {
			return err
		}
		return <-done
	}
	if err := handshake(&tls.Config{RootCAs: pool, ServerName: "temporal", Certificates: []tls.Certificate{client}, MinVersion: tls.VersionTLS12}); err != nil {
		t.Errorf("mTLS với chứng chỉ client: %v", err)
	}
	if err := handshake(&tls.Config{RootCAs: pool, ServerName: "temporal", MinVersion: tls.VersionTLS12}); err == nil {
		t.Error("không có chứng chỉ client phải bị từ chối")
	}
	if err := handshake(&tls.Config{RootCAs: pool, ServerName: "khac", Certificates: []tls.Certificate{client}, MinVersion: tls.VersionTLS12}); err == nil {
		t.Error("tên server khác phải bị từ chối")
	}

	// Chạy lại: không đổi gì (process đang chạy không phải khởi động lại sau mỗi lần deploy).
	before, _ := os.ReadFile(filepath.Join(dir, "server/cert.pem"))
	if changed, err := ensureCerts(cfg); err != nil || len(changed) != 0 {
		t.Errorf("chạy lại: %v, %v; muốn không đổi", changed, err)
	}
	// Gần hết hạn: chỉ chứng chỉ lá được cấp lại, CA giữ nguyên.
	later := cfg
	later.Now = now.Add(leafValidity - 30*24*time.Hour)
	changed, err = ensureCerts(later)
	if err != nil || len(changed) != 2 {
		t.Fatalf("gần hết hạn: %v, %v; muốn server + client", changed, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "server/cert.pem"))
	if string(before) == string(after) {
		t.Error("chứng chỉ server phải được cấp lại")
	}
	if ca2, _ := os.ReadFile(filepath.Join(dir, "ca.pem")); string(ca2) != string(caPEM) {
		t.Error("CA không được đổi khi cấp lại chứng chỉ lá")
	}
	// Đổi tên server: chứng chỉ server được cấp lại cho tên mới.
	renamed := later
	renamed.ServerName = "temporal-frontend"
	if changed, err := ensureCerts(renamed); err != nil || len(changed) != 1 || changed[0] != "server" {
		t.Errorf("đổi tên server: %v, %v", changed, err)
	}
	// CA sắp hết hạn: dừng, không lặng lẽ đổi CA.
	expired := cfg
	expired.Now = now.Add(caValidity)
	if _, err := ensureCerts(expired); err == nil {
		t.Error("CA hết hạn phải báo lỗi")
	}
	// Khoá hỏng: dừng, không ghi đè.
	if err := os.Chmod(filepath.Join(dir, "ca.key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("hỏng"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCerts(cfg); err == nil {
		t.Error("khoá CA hỏng phải báo lỗi")
	}
}
