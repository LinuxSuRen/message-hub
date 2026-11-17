// Package source 实现外部信息源（首个支持 GitHub 仓库监视）：
// webhook 实时事件接入、star/fork 轮询、条目去重与订阅分发。
package source

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// Encrypt 以密钥派生 AES-GCM 加密明文，输出 base64(nonce||密文)。
// 密钥与会话签名密钥同源（经 SHA-256 派生 32 字节）。
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解开 Encrypt 的输出；密钥不符或数据损坏时报错。
func Decrypt(key []byte, encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("密文长度不合法")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("解密失败: %w", err)
	}
	return string(plain), nil
}

// deriveKey 从任意长度的主密钥派生 32 字节 AES 密钥。
func deriveKey(master []byte) []byte {
	sum := sha256.Sum256(master)
	return sum[:]
}
