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

// encryptToken seals value with AES-256-GCM. aad (additional authenticated
// data) binds the ciphertext to where it is stored (user + reference), so a
// row copied to another user or reference fails to decrypt instead of
// silently handing one account's token to another.
func encryptToken(key [32]byte, value string, aad []byte) ([]byte, []byte, error) {
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
	ciphertext := aead.Seal(nil, nonce, []byte(value), aad)
	return nonce, ciphertext, nil
}

// decryptToken opens a ciphertext sealed with the given aad. When
// allowLegacy is set and the aad-bound open fails, it retries without aad so
// tokens written before aad binding still decrypt; they are re-sealed with
// aad on their next write.
func decryptToken(key [32]byte, nonce, ciphertext, aad []byte, allowLegacy bool) (string, error) {
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
	plain, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil && allowLegacy && len(aad) > 0 {
		plain, err = aead.Open(nil, nonce, ciphertext, nil)
	}
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
