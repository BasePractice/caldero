package services

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ErrNoMasterKey возвращается, когда мастер-ключ не настроен.
var ErrNoMasterKey = errors.New("master key is not configured")

// Cipher шифрует данные, которые нельзя хранить в БД открытым текстом.
// AES-GCM: он даёт не только шифрование, но и проверку целостности —
// подмена шифротекста обнаруживается при расшифровке.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher строит шифр из мастер-ключа. Ключ произвольной длины сводится
// к 32 байтам через SHA-256: требовать от оператора ровно 32 байта
// неудобно и приводит к тому, что ключ подбирают под длину, а не под стойкость.
func NewCipher(masterKey string) (*Cipher, error) {
	if masterKey == "" {
		return nil, ErrNoMasterKey
	}
	sum := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("creating aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt возвращает nonce и шифротекст одной строкой байт.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	// Nonce не секрет, но обязан быть уникальным для каждого шифрования:
	// его повторное использование с тем же ключом раскрывает открытый текст.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext is shorter than nonce")
	}
	plaintext, err := c.aead.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	return plaintext, nil
}
