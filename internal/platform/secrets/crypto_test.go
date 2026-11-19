package secrets

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestSealOpen(t *testing.T) {
	rootHex, pub, err := GenerateLocalRootKey()
	if err != nil {
		t.Fatal(err)
	}
	root, _ := hex.DecodeString(rootHex)
	sealer, err := NewLocalSealer(pub, "local")
	if err != nil {
		t.Fatal(err)
	}
	opener, err := NewLocalOpener(root, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if opener.PublicKey() != pub {
		t.Error("khoá công khai dẫn xuất từ khoá gốc không khớp")
	}
	b := Binding{OrgID: uuid.New(), SecretID: uuid.New(), Version: 1}
	secret := []byte("sk-ant-test-0123456789")
	s, err := sealer.Seal(b, secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.Ciphertext, secret) || bytes.Contains(s.WrappedDEK, secret) {
		t.Fatal("plaintext nằm trong bản mã")
	}
	got, err := opener.Open(b, s)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("open: %q %v", got, err)
	}
	// Mỗi lần mã hoá cho bản mã khác nhau.
	s2, _ := sealer.Seal(b, secret)
	if bytes.Equal(s.Ciphertext, s2.Ciphertext) || bytes.Equal(s.WrappedDEK, s2.WrappedDEK) {
		t.Error("hai lần mã hoá cho cùng bản mã")
	}

	otherRootHex, _, _ := GenerateLocalRootKey()
	otherRoot, _ := hex.DecodeString(otherRootHex)
	otherOpener, _ := NewLocalOpener(otherRoot, "local")
	flip := func(in []byte, i int) []byte { out := bytes.Clone(in); out[i] ^= 1; return out }
	for name, c := range map[string]struct {
		o Opener
		b Binding
		s Sealed
	}{
		"org khác":            {opener, Binding{OrgID: uuid.New(), SecretID: b.SecretID, Version: 1}, s},
		"secret khác":         {opener, Binding{OrgID: b.OrgID, SecretID: uuid.New(), Version: 1}, s},
		"version khác":        {opener, Binding{OrgID: b.OrgID, SecretID: b.SecretID, Version: 2}, s},
		"khoá gốc khác":       {otherOpener, b, s},
		"ciphertext bị sửa":   {opener, b, Sealed{flip(s.Ciphertext, len(s.Ciphertext)-1), s.WrappedDEK, s.KEKRef}},
		"wrapped DEK bị sửa":  {opener, b, Sealed{s.Ciphertext, flip(s.WrappedDEK, 40), s.KEKRef}},
		"DEK của secret khác": {opener, b, Sealed{s.Ciphertext, s2.WrappedDEK, s.KEKRef}},
		"kek_ref khác":        {opener, b, Sealed{s.Ciphertext, s.WrappedDEK, "aws-kms:abc"}},
		"rỗng":                {opener, b, Sealed{}},
	} {
		if got, err := c.o.Open(c.b, c.s); err == nil {
			t.Errorf("%s: giải mã được (%q)", name, got)
		}
	}
}

func TestLocalProviderRefusesProduction(t *testing.T) {
	rootHex, pub, _ := GenerateLocalRootKey()
	root, _ := hex.DecodeString(rootHex)
	if _, err := NewLocalSealer(pub, "production"); err == nil {
		t.Error("sealer cục bộ chạy được ở production")
	}
	if _, err := NewLocalOpener(root, "production"); err == nil {
		t.Error("opener cục bộ chạy được ở production")
	}
	if _, err := NewLocalSealer("not-base64!", "local"); err == nil {
		t.Error("khoá công khai sai được chấp nhận")
	}
	if _, err := NewLocalOpener([]byte("short"), "local"); err == nil {
		t.Error("khoá gốc ngắn được chấp nhận")
	}
}

func TestLoadLocalRootKey(t *testing.T) {
	rootHex, _, _ := GenerateLocalRootKey()
	dir := t.TempDir()
	good := filepath.Join(dir, "root.key")
	if err := os.WriteFile(good, []byte(rootHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if root, err := LoadLocalRootKey(good); err != nil || len(root) != 32 {
		t.Errorf("đọc khoá: %v", err)
	}
	open := filepath.Join(dir, "open.key")
	_ = os.WriteFile(open, []byte(rootHex), 0o644) //nolint:gosec // cố ý: kiểm tra việc từ chối
	if _, err := LoadLocalRootKey(open); err == nil {
		t.Error("file khoá ai cũng đọc được vẫn được chấp nhận")
	}
	bad := filepath.Join(dir, "bad.key")
	_ = os.WriteFile(bad, []byte("zz"), 0o600)
	if _, err := LoadLocalRootKey(bad); err == nil {
		t.Error("file khoá sai định dạng được chấp nhận")
	}
	if _, err := LoadLocalRootKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("file không tồn tại được chấp nhận")
	}
}
