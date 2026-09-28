package backend

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPasswordSecretCipherRoundTrip(t *testing.T) {
	dir := t.TempDir()
	box, err := loadOrCreatePasswordSecretCipher(dir, false)
	if err != nil {
		t.Fatalf("loadOrCreatePasswordSecretCipher: %v", err)
	}

	for _, password := range []string{"", "correct horse battery staple"} {
		secret, err := box.encrypt("user-1", password)
		if err != nil {
			t.Fatalf("encrypt %q: %v", password, err)
		}
		if secret == "" || !strings.HasPrefix(secret, passwordSecretV1) {
			t.Fatalf("encrypted secret = %q, want non-empty %q value", secret, passwordSecretV1)
		}
		if password != "" && strings.Contains(secret, password) {
			t.Fatalf("encrypted secret contains plaintext password")
		}
		plain, err := box.decrypt("user-1", secret)
		if err != nil {
			t.Fatalf("decrypt %q: %v", password, err)
		}
		if plain != password {
			t.Fatalf("round trip = %q, want %q", plain, password)
		}
	}

	first, err := box.encrypt("user-1", "same-password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.encrypt("user-1", "same-password")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("encrypting the same password twice reused a nonce/ciphertext")
	}
}

func TestPasswordSecretCipherFailsClosed(t *testing.T) {
	keyA := make([]byte, passwordSecretKeySize)
	keyB := make([]byte, passwordSecretKeySize)
	for i := range keyA {
		keyA[i] = byte(i + 1)
		keyB[i] = byte(i + 2)
	}
	boxA, err := passwordSecretCipherFromKey("a", keyA)
	if err != nil {
		t.Fatal(err)
	}
	boxB, err := passwordSecretCipherFromKey("b", keyB)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := boxA.encrypt("user-a", "password-1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := boxA.decrypt("user-b", secret); err == nil {
		t.Fatal("decrypt with the wrong user ID/AAD unexpectedly succeeded")
	}
	if _, err := boxB.decrypt("user-a", secret); err == nil {
		t.Fatal("decrypt with the wrong master key unexpectedly succeeded")
	}
	if _, err := boxA.decrypt("user-a", ""); err == nil {
		t.Fatal("empty password_secret must be treated as corrupt/uninitialized")
	}

	encoded := strings.TrimPrefix(secret, passwordSecretV1)
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 0x01
	tampered := passwordSecretV1 + base64.StdEncoding.EncodeToString(payload)
	if _, err := boxA.decrypt("user-a", tampered); err == nil {
		t.Fatal("tampered ciphertext unexpectedly decrypted")
	}
}

func TestPasswordSecretMasterKeyLifecycle(t *testing.T) {
	dir := t.TempDir()
	box, err := loadOrCreatePasswordSecretCipher(dir, false)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyPath := filepath.Join(dir, userPasswordKeyFile)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if len(key) != passwordSecretKeySize {
		t.Fatalf("key length = %d, want %d", len(key), passwordSecretKeySize)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("key mode = %o, want 600", got)
		}
	}

	secret, err := box.encrypt("user-1", "persisted")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadOrCreatePasswordSecretCipher(dir, true)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if plain, err := reloaded.decrypt("user-1", secret); err != nil || plain != "persisted" {
		t.Fatalf("reloaded key decrypt = %q, %v", plain, err)
	}

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreatePasswordSecretCipher(dir, true); err == nil {
		t.Fatal("missing key with encrypted records must fail closed")
	}
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreatePasswordSecretCipher(dir, true); err == nil {
		t.Fatal("corrupt key with encrypted records must fail closed")
	}
}

func TestUserStoreFreshSchemaPersistsRecoverablePasswordSecret(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mappings.db")
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	store, err := NewUserStore(db, nil)
	if err != nil {
		_ = closeSQLite(db)
		t.Fatalf("NewUserStore: %v", err)
	}

	wantPasswords := map[string]string{}
	for _, tc := range []struct {
		username string
		password string
	}{
		{username: "empty", password: ""},
		{username: "normal", password: "12345678"},
	} {
		user, err := store.Create(tc.username, tc.password, nil)
		if err != nil {
			_ = closeSQLite(db)
			t.Fatalf("Create(%s): %v", tc.username, err)
		}
		if user.PasswordHash == "" {
			t.Fatalf("Create(%s) stored an empty password hash", tc.username)
		}
		if user.PasswordSecret == "" || !strings.HasPrefix(user.PasswordSecret, passwordSecretV1) {
			t.Fatalf("Create(%s) secret = %q", tc.username, user.PasswordSecret)
		}
		plain, err := store.passwordPlaintext(user)
		if err != nil || plain != tc.password {
			t.Fatalf("Create(%s) recoverable password = %q, %v", tc.username, plain, err)
		}
		wantPasswords[user.ID] = tc.password
	}
	if err := closeSQLite(db); err != nil {
		t.Fatalf("closeSQLite: %v", err)
	}

	db2, err := openSQLite(dbPath)
	if err != nil {
		t.Fatalf("reopen SQLite: %v", err)
	}
	defer func() { _ = closeSQLite(db2) }()
	reopened, err := NewUserStore(db2, nil)
	if err != nil {
		t.Fatalf("NewUserStore reopen: %v", err)
	}
	for userID, want := range wantPasswords {
		user := reopened.Get(userID)
		if user == nil {
			t.Fatalf("reopened user %s missing", userID)
		}
		plain, err := reopened.passwordPlaintext(user)
		if err != nil || plain != want {
			t.Fatalf("reopened password for %s = %q, %v; want %q", userID, plain, err, want)
		}
	}
}
