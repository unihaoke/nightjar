package utils

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Cipher 使用 AES-256-GCM 加解密敏感数据（被管中间件连接密码、AI key、Git 凭据）。
//
// 密钥来源（见 6.3）：security.master_key 环境变量 > 主密钥文件（0600）。
// 密文格式为 base64(nonce || ciphertext)，便于直接落库。
type Cipher struct {
	aead cipher.AEAD
	// keyID 为密钥指纹，便于轮换期间识别密文版本。
	keyID string
}

// NewCipher 根据主密钥材料构造 Cipher。
//
// 传入材料先经 SHA-256 派生 32 字节密钥，允许主密钥为任意长度字符串。
func NewCipher(masterKey string) (*Cipher, error) {
	if strings.TrimSpace(masterKey) == "" {
		return nil, errors.New("master key is empty")
	}
	sum := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &Cipher{aead: aead, keyID: base64.RawURLEncoding.EncodeToString(sum[:4])}, nil
}

// LoadOrCreateMasterKey 读取主密钥：优先环境变量/配置，其次密钥文件，文件不存在则生成。
//
// 生成的文件权限为 0600，满足 6.3 的密钥管理要求。
func LoadOrCreateMasterKey(configured, filePath string) (string, string, error) {
	if strings.TrimSpace(configured) != "" {
		return configured, "config", nil
	}
	if filePath == "" {
		return "", "", errors.New("master key file path is empty")
	}
	data, err := os.ReadFile(filePath)
	if err == nil {
		key := strings.TrimSpace(string(data))
		if key != "" {
			return key, filePath, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("read master key: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", "", fmt.Errorf("generate master key: %w", err)
	}
	key := base64.RawStdEncoding.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return "", "", fmt.Errorf("create key dir: %w", err)
	}
	if err := os.WriteFile(filePath, []byte(key), 0o600); err != nil {
		return "", "", fmt.Errorf("write master key: %w", err)
	}
	return key, filePath, nil
}

// KeyID 返回当前密钥指纹。
func (c *Cipher) KeyID() string { return c.keyID }

// Encrypt 加密明文，空明文返回空串（表示未配置密码）。
func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密密文，空密文返回空串。
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}

// MaskSecret 返回脱敏后的密钥展示串（仅保留首尾各 2 位）。
func MaskSecret(secret string) string {
	if len(secret) <= 4 {
		return "****"
	}
	return secret[:2] + strings.Repeat("*", 6) + secret[len(secret)-2:]
}
