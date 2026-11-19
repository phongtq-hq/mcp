// Package secrets lưu secret bằng envelope encryption (ADR-0008): mỗi secret được mã
// hoá AES-256-GCM bằng một DEK riêng, và DEK được bọc bởi một lớp khoá nằm ngoài
// database (KeyWrapper, ADR-0012).
//
// Phân quyền theo process là một phần của thiết kế, và được thể hiện ngay ở kiểu:
//   - Sealer chỉ mã hoá được. Process api giữ Sealer: nó ghi secret nhưng không đọc
//     lại được plaintext.
//   - Opener giải mã được. Chỉ process gateway giữ Opener.
//
// Plaintext không bao giờ được ghi vào log, lỗi, audit hay phản hồi API.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
)

// MaxPlaintextBytes là trần kích thước của một secret.
const MaxPlaintextBytes = 16 << 10

// Sealed là một secret đã mã hoá, như được lưu trong database.
type Sealed struct {
	Ciphertext []byte // nonce || AES-256-GCM(DEK, plaintext)
	WrappedDEK []byte // DEK đã bọc; định dạng do KeyWrapper quyết định
	KEKRef     string // định danh provider và khoá đã bọc DEK
}

// Binding gắn một bản mã với đúng một secret của đúng một org: bản mã chép sang
// dòng khác (org khác, secret khác, version khác) không giải mã được.
type Binding struct {
	OrgID    uuid.UUID // uuid.Nil cho secret của platform
	SecretID uuid.UUID
	Version  int
}

func (b Binding) aad() []byte {
	return []byte(fmt.Sprintf("hq-secret/v1|%s|%s|%d", b.OrgID, b.SecretID, b.Version))
}

// Sealer mã hoá secret. Không có đường nào từ Sealer ra plaintext.
type Sealer interface {
	Seal(b Binding, plaintext []byte) (Sealed, error)
}

// Opener giải mã secret.
type Opener interface {
	Open(b Binding, s Sealed) ([]byte, error)
}

// ErrOpen báo bản mã không giải mã được (sai khoá, sai ràng buộc, hoặc bị sửa).
var ErrOpen = errors.New("secrets: không giải mã được")

const (
	localScheme  = "local-x25519-v1"
	rootKeyBytes = 32
	envProd      = "production"
)

// errProduction: provider cục bộ không có kiểm soát truy cập và audit độc lập trên
// khoá gốc, nên không được giữ credential production (ADR-0012).
var errProduction = errors.New("secrets: provider khoá cục bộ không được dùng khi HQ_ENV=production; cần provider Cloud KMS (ADR-0012)")

func localPrivate(root []byte) (*ecdh.PrivateKey, error) {
	if len(root) != rootKeyBytes {
		return nil, errors.New("secrets: khoá gốc phải dài đúng 32 byte")
	}
	seed, err := hkdf.Key(sha256.New, root, nil, "hq-secrets/local/x25519/v1", 32)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(seed)
}

func kekRef(pub *ecdh.PublicKey) string {
	sum := sha256.Sum256(pub.Bytes())
	return localScheme + ":" + hex.EncodeToString(sum[:8])
}

// GenerateLocalRootKey tạo khoá gốc mới và trả về nó (hex) cùng khoá công khai
// tương ứng (base64) để cấp cho process chỉ mã hoá.
func GenerateLocalRootKey() (rootHex, publicKey string, err error) {
	root := make([]byte, rootKeyBytes)
	if _, err := rand.Read(root); err != nil {
		return "", "", err
	}
	priv, err := localPrivate(root)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(root), base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

// LocalSealer mã hoá bằng khoá công khai của provider cục bộ.
type LocalSealer struct {
	pub *ecdh.PublicKey
	ref string
}

// NewLocalSealer nhận khoá công khai (base64) do GenerateLocalRootKey in ra.
func NewLocalSealer(publicKey, env string) (*LocalSealer, error) {
	if env == envProd {
		return nil, errProduction
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil {
		return nil, errors.New("secrets: khoá công khai phải là base64")
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("secrets: khoá công khai không hợp lệ")
	}
	return &LocalSealer{pub: pub, ref: kekRef(pub)}, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, ErrOpen
	}
	out, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}

// wrapKey dẫn xuất khoá bọc DEK từ bí mật chung ECDH. org_id nằm trong thông tin dẫn
// xuất, nên mỗi org có khoá bọc riêng (vai trò của KEK theo org).
func wrapKey(shared []byte, eph, pub *ecdh.PublicKey, orgID uuid.UUID) ([]byte, error) {
	salt := append(append([]byte{}, eph.Bytes()...), pub.Bytes()...)
	return hkdf.Key(sha256.New, shared, salt, "hq-secrets/wrap/v1|"+orgID.String(), 32)
}

// Seal hiện thực Sealer.
func (s *LocalSealer) Seal(b Binding, plaintext []byte) (Sealed, error) {
	if len(plaintext) == 0 || len(plaintext) > MaxPlaintextBytes {
		return Sealed{}, errors.New("secrets: kích thước secret không hợp lệ")
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, err
	}
	ct, err := seal(dek, plaintext, b.aad())
	if err != nil {
		return Sealed{}, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Sealed{}, err
	}
	shared, err := eph.ECDH(s.pub)
	if err != nil {
		return Sealed{}, err
	}
	kw, err := wrapKey(shared, eph.PublicKey(), s.pub, b.OrgID)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := seal(kw, dek, b.aad())
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{Ciphertext: ct, WrappedDEK: append(eph.PublicKey().Bytes(), wrapped...), KEKRef: s.ref}, nil
}

// LocalOpener giải mã bằng khoá gốc của provider cục bộ.
type LocalOpener struct {
	priv *ecdh.PrivateKey
	ref  string
}

// NewLocalOpener nhận khoá gốc (32 byte).
func NewLocalOpener(root []byte, env string) (*LocalOpener, error) {
	if env == envProd {
		return nil, errProduction
	}
	priv, err := localPrivate(root)
	if err != nil {
		return nil, err
	}
	return &LocalOpener{priv: priv, ref: kekRef(priv.PublicKey())}, nil
}

// LoadLocalRootKey đọc khoá gốc (hex) từ file do hạ tầng cấp. File mà nhóm hoặc
// người khác đọc được bị từ chối.
func LoadLocalRootKey(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // đường dẫn do người vận hành cấu hình
	if err != nil {
		return nil, errors.New("secrets: không đọc được file khoá gốc")
	}
	defer func() { _ = f.Close() }()
	// Kiểm trên chính file đã mở, không phải trên đường dẫn.
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("secrets: file khoá gốc phải là file thường")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secrets: file khoá gốc phải chỉ chủ sở hữu đọc được (chmod 600)")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1024))
	if err != nil {
		return nil, errors.New("secrets: không đọc được file khoá gốc")
	}
	root, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(root) != rootKeyBytes {
		return nil, errors.New("secrets: file khoá gốc phải chứa 32 byte dạng hex")
	}
	return root, nil
}

// PublicKey trả về khoá công khai (base64) ứng với khoá gốc này.
func (o *LocalOpener) PublicKey() string {
	return base64.StdEncoding.EncodeToString(o.priv.PublicKey().Bytes())
}

// Open hiện thực Opener.
func (o *LocalOpener) Open(b Binding, s Sealed) ([]byte, error) {
	if s.KEKRef != o.ref || len(s.WrappedDEK) <= 32 {
		return nil, ErrOpen
	}
	eph, err := ecdh.X25519().NewPublicKey(s.WrappedDEK[:32])
	if err != nil {
		return nil, ErrOpen
	}
	shared, err := o.priv.ECDH(eph)
	if err != nil {
		return nil, ErrOpen
	}
	kw, err := wrapKey(shared, eph, o.priv.PublicKey(), b.OrgID)
	if err != nil {
		return nil, ErrOpen
	}
	dek, err := open(kw, s.WrappedDEK[32:], b.aad())
	if err != nil {
		return nil, ErrOpen
	}
	return open(dek, s.Ciphertext, b.aad())
}
