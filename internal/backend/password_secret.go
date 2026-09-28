package backend

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	userPasswordKeyFile   = "user-password.key"
	passwordSecretV1      = "v1:"
	passwordSecretKeySize = 32
)

type passwordSecretCipher struct {
	keyPath string
	aead    cipher.AEAD
}

func loadOrCreatePasswordSecretCipher(dataDir string, encryptedRecordsExist bool) (*passwordSecretCipher, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("password secret: data directory is empty")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("password secret: create data directory: %w", err)
	}

	keyPath := filepath.Join(dataDir, userPasswordKeyFile)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("password secret: read master key: %w", err)
		}
		if encryptedRecordsExist {
			return nil, fmt.Errorf("password secret: master key missing while encrypted user passwords exist")
		}
		key = make([]byte, passwordSecretKeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("password secret: generate master key: %w", err)
		}
		if err := WriteFileAtomic(keyPath, key, 0o600); err != nil {
			return nil, fmt.Errorf("password secret: persist master key: %w", err)
		}
	} else if len(key) != passwordSecretKeySize {
		return nil, fmt.Errorf("password secret: invalid master key length %d", len(key))
	}

	box, err := passwordSecretCipherFromKey(keyPath, key)
	if err != nil {
		return nil, err
	}
	return box, nil
}

func passwordSecretCipherFromKey(keyPath string, key []byte) (*passwordSecretCipher, error) {
	if len(key) != passwordSecretKeySize {
		return nil, fmt.Errorf("password secret: invalid master key length %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("password secret: initialize AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("password secret: initialize GCM: %w", err)
	}
	return &passwordSecretCipher{keyPath: keyPath, aead: aead}, nil
}

func (c *passwordSecretCipher) encrypt(userID, password string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("password secret: cipher unavailable")
	}
	if userID == "" {
		return "", errors.New("password secret: user id is empty")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("password secret: generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, []byte(password), []byte(userID))
	payload := make([]byte, 0, len(nonce)+len(sealed))
	payload = append(payload, nonce...)
	payload = append(payload, sealed...)
	return passwordSecretV1 + base64.StdEncoding.EncodeToString(payload), nil
}

func (c *passwordSecretCipher) decrypt(userID, secret string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("password secret: cipher unavailable")
	}
	if userID == "" {
		return "", errors.New("password secret: user id is empty")
	}
	if secret == "" {
		return "", errors.New("password secret: empty encrypted value")
	}
	if !strings.HasPrefix(secret, passwordSecretV1) {
		return "", errors.New("password secret: unsupported encrypted value version")
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, passwordSecretV1))
	if err != nil {
		return "", fmt.Errorf("password secret: decode encrypted value: %w", err)
	}
	if len(payload) < c.aead.NonceSize()+c.aead.Overhead() {
		return "", errors.New("password secret: encrypted value is truncated")
	}
	nonce := payload[:c.aead.NonceSize()]
	ciphertext := payload[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(userID))
	if err != nil {
		return "", fmt.Errorf("password secret: decrypt encrypted value: %w", err)
	}
	return string(plaintext), nil
}
