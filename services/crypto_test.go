package services

import (
	"bytes"
	"errors"
	"testing"
)

func TestCipherRoundTrip(t *testing.T) {
	cipher, err := NewCipher("мастер-ключ-для-теста")
	if err != nil {
		t.Fatalf("не удалось создать шифр: %v", err)
	}
	secret := []byte("приватный ключ подписи")

	encrypted, err := cipher.Encrypt(secret)
	if err != nil {
		t.Fatalf("шифрование: %v", err)
	}
	if bytes.Contains(encrypted, secret) {
		t.Fatal("открытый текст виден в шифротексте")
	}

	decrypted, err := cipher.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("расшифровка: %v", err)
	}
	if !bytes.Equal(decrypted, secret) {
		t.Fatalf("расшифровано %q, ожидалось %q", decrypted, secret)
	}
}

func TestCipherNonceIsUnique(t *testing.T) {
	cipher, err := NewCipher("мастер-ключ-для-теста")
	if err != nil {
		t.Fatalf("не удалось создать шифр: %v", err)
	}

	// Одинаковый открытый текст обязан давать разный шифротекст: иначе
	// по совпадению записей видно, что ключи одинаковые.
	first, err := cipher.Encrypt([]byte("одно и то же"))
	if err != nil {
		t.Fatalf("шифрование: %v", err)
	}
	second, err := cipher.Encrypt([]byte("одно и то же"))
	if err != nil {
		t.Fatalf("шифрование: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("два шифрования дали одинаковый результат — nonce не уникален")
	}
}

func TestCipherRejectsTampering(t *testing.T) {
	cipher, err := NewCipher("мастер-ключ-для-теста")
	if err != nil {
		t.Fatalf("не удалось создать шифр: %v", err)
	}
	encrypted, err := cipher.Encrypt([]byte("приватный ключ"))
	if err != nil {
		t.Fatalf("шифрование: %v", err)
	}

	tampered := bytes.Clone(encrypted)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err = cipher.Decrypt(tampered); err == nil {
		t.Fatal("подмена шифротекста не обнаружена")
	}
}

func TestCipherRejectsWrongKey(t *testing.T) {
	source, err := NewCipher("первый ключ")
	if err != nil {
		t.Fatalf("не удалось создать шифр: %v", err)
	}
	other, err := NewCipher("второй ключ")
	if err != nil {
		t.Fatalf("не удалось создать шифр: %v", err)
	}

	encrypted, err := source.Encrypt([]byte("секрет"))
	if err != nil {
		t.Fatalf("шифрование: %v", err)
	}
	if _, err = other.Decrypt(encrypted); err == nil {
		t.Fatal("расшифровка чужим ключом не должна проходить")
	}
}

func TestNewCipherRequiresMasterKey(t *testing.T) {
	if _, err := NewCipher(""); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("ожидалась ErrNoMasterKey, получено %v", err)
	}
}
