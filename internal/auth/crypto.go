// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// AES-256-GCM envelope encryption for secrets at rest (mfa_secret bytea).
// Blob layout: nonce (12B) || ciphertext+tag.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

var ErrDecrypt = errors.New("decrypt_failed")

func encryptBlob(key, plaintext []byte) ([]byte, error) {
	gcm, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptBlob(key, blob []byte) ([]byte, error) {
	gcm, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrDecrypt
	}
	plain, err := gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("data key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
