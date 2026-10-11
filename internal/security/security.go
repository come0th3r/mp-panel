package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/crypto/argon2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func PasswordHash(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	sum := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(sum)
}
func PasswordOK(password, hash string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[1])
	if e != nil || len(salt) != 16 {
		return false
	}
	want, e := base64.RawStdEncoding.DecodeString(parts[2])
	if e != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(want, got) == 1
}
func NewKey(path string) ([]byte, error) {
	b, e := os.ReadFile(path)
	if e == nil {
		if len(b) != 32 {
			return nil, errors.New("invalid encryption key")
		}
		return b, nil
	}
	if !os.IsNotExist(e) {
		return nil, e
	}
	b = make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(e) {
		return os.ReadFile(path)
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	_, e = f.Write(b)
	return b, e
}

type Vault struct{ Key []byte }

func OpenVault(dir string) (*Vault, error) {
	if e := os.MkdirAll(filepath.Join(dir, "keys"), 0700); e != nil {
		return nil, e
	}
	key, e := NewKey(filepath.Join(dir, "keys", "secrets.key"))
	return &Vault{Key: key}, e
}
func (v *Vault) Encrypt(plain []byte) ([]byte, error) {
	block, e := aes.NewCipher(v.Key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, g.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return g.Seal(nonce, nonce, plain, []byte("mp-v1")), nil
}
func (v *Vault) Decrypt(data []byte) ([]byte, error) {
	block, e := aes.NewCipher(v.Key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	if len(data) < g.NonceSize() {
		return nil, errors.New("invalid encrypted secret")
	}
	return g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], []byte("mp-v1"))
}
func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
func TOTPCounter(secret, code string, last int64) (int64, error) {
	if len(code) != 6 {
		return -1, errors.New("invalid TOTP")
	}
	if _, e := strconv.Atoi(code); e != nil {
		return -1, e
	}
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if e != nil {
		return -1, e
	}
	now := time.Now().Unix() / 30
	for _, counter := range []int64{now, now - 1, now + 1} {
		if counter <= last {
			continue
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(counter))
		h := hmac.New(sha1.New, key)
		h.Write(b[:])
		sum := h.Sum(nil)
		o := sum[len(sum)-1] & 15
		n := (binary.BigEndian.Uint32(sum[o:o+4]) & 0x7fffffff) % 1000000
		if subtle.ConstantTimeCompare([]byte(fmt.Sprintf("%06d", n)), []byte(code)) == 1 {
			return counter, nil
		}
	}
	return -1, errors.New("invalid or already used TOTP")
}
