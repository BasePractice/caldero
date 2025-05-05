package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"errors"
	"sync"
)

type KeyManager interface {
	RotateKeys(ctx context.Context) error
	GetPublicKey(ctx context.Context, kid string) (any, error)
	GetPrivateKey(ctx context.Context) (any, error)
	GetPublicKeyId(ctx context.Context) (string, error)
	GetKeys(ctx context.Context) ([]Key, error)
}

type km struct {
	mu           sync.RWMutex
	db           DatabaseUsers
	currentKeyId string
}

func (km *km) GetKeys(ctx context.Context) ([]Key, error) {
	var keys []Key
	err := km.db.GetKeys(ctx, func(id string, bytes []byte) {
		keys = append(keys, Key{
			Id:   id,
			Data: bytes,
		})
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

type Key struct {
	Id   string
	Data []byte
}

func NewKeyManager(ctx context.Context, db DatabaseUsers) (KeyManager, error) {
	var k = km{mu: sync.RWMutex{}, db: db}
	err := k.initialize(ctx)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (km *km) initialize(ctx context.Context) error {
	keyId, err := km.db.GetLastKey(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if keyId != "" {
		km.mu.Lock()
		km.currentKeyId = keyId
		km.mu.Unlock()
		return nil
	}

	return km.RotateKeys(ctx)
}

func (km *km) RotateKeys(ctx context.Context) error {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	privateBytes := x509.MarshalPKCS1PrivateKey(privateKey)

	keyId, err := km.db.CreateKey(ctx, privateBytes)
	if err != nil {
		return err
	}
	km.mu.Lock()
	km.currentKeyId = keyId
	km.mu.Unlock()
	return km.db.ClearKeys(ctx)
}

func (km *km) GetPublicKey(ctx context.Context, kid string) (any, error) {
	if kid == "" {
		kid = km.currentKeyId
	}

	privateBytes, err := km.db.GetKey(ctx, kid)
	if err != nil {
		return nil, err
	}

	privateKey, err := x509.ParsePKCS1PrivateKey(privateBytes)
	if err != nil {
		return nil, err
	}

	return &privateKey.PublicKey, nil
}

func (km *km) GetPrivateKey(ctx context.Context) (any, error) {
	km.mu.RLock()
	defer km.mu.RUnlock()

	privateBytes, err := km.db.GetKey(ctx, km.currentKeyId)
	if err != nil {
		return nil, err
	}

	return x509.ParsePKCS1PrivateKey(privateBytes)
}

func (km *km) GetPublicKeyId(_ context.Context) (string, error) {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.currentKeyId, nil
}
