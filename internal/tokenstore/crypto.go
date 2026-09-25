package tokenstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

const tokenNonceSize = 12

func deriveTokenKey(material string) ([32]byte, error) {
	if material == "" {
		return [32]byte{}, errors.New("tokenstore: empty key")
	}
	return sha256.Sum256([]byte(material)), nil
}

func encryptToken(key [32]byte, value string) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ciphertext := aead.Seal(nil, nonce, []byte(value), nil)
	return nonce, ciphertext, nil
}

func decryptToken(key [32]byte, nonce, ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(nonce) != aead.NonceSize() {
		return "", fmt.Errorf("bad nonce length %d", len(nonce))
	}
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
