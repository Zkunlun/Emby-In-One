package backend

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPhase2JUserStoredCredentialsAreJSONHidden(t *testing.T) {
	user := &User{
		ID:             "user-1",
		Username:       "alice",
		PasswordHash:   "PHASE2J-HASH-MUST-NOT-LEAK",
		PasswordSecret: "PHASE2J-SECRET-MUST-NOT-LEAK",
		Enabled:        true,
	}

	payload, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("marshal user: %v", err)
	}
	body := string(payload)
	for _, forbidden := range []string{
		"PHASE2J-HASH-MUST-NOT-LEAK",
		"PHASE2J-SECRET-MUST-NOT-LEAK",
		"PasswordHash",
		"PasswordSecret",
		"passwordHash",
		"passwordSecret",
		"password_hash",
		"password_secret",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("serialized User exposed stored credential material %q: %s", forbidden, body)
		}
	}
}
