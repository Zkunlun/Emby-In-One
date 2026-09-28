package backend

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPhase2IAuthenticateByNameWrongPasswordBoundaries(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		emptyUser, err := app.UserStore.Create("phase2i-empty", "", nil)
		if err != nil {
			t.Fatalf("create empty-password user: %v", err)
		}
		normalUser, err := app.UserStore.Create("phase2i-normal", "password123", nil)
		if err != nil {
			t.Fatalf("create nonempty-password user: %v", err)
		}

		cases := []struct {
			name     string
			username string
			password string
			want     int
		}{
			{name: "empty user accepts empty password", username: emptyUser.Username, password: "", want: http.StatusOK},
			{name: "empty user rejects wrong password", username: emptyUser.Username, password: "wrong-password", want: http.StatusUnauthorized},
			{name: "nonempty user rejects empty password", username: normalUser.Username, password: "", want: http.StatusUnauthorized},
			{name: "unknown user rejects empty password", username: "phase2i-unknown", password: "", want: http.StatusUnauthorized},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
					"Username": tc.username,
					"Pw":       tc.password,
				}, "")
				if rr.Code != tc.want {
					t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.want, rr.Body.String())
				}
			})
		}
	})
}

func TestPhase2IAdminPasswordPolicyRemainsNonEmpty(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")
		beforeHash := app.ConfigStore.Snapshot().Admin.Password

		emptyWrite := doJSONRequest(t, handler, http.MethodPut, "/admin/api/settings", map[string]any{
			"currentPassword": "secret",
			"adminPassword":   "",
		}, adminToken)
		if emptyWrite.Code != http.StatusOK {
			t.Fatalf("empty admin password update status = %d, want 200 no-op; body=%s", emptyWrite.Code, emptyWrite.Body.String())
		}
		if afterHash := app.ConfigStore.Snapshot().Admin.Password; afterHash != beforeHash {
			t.Fatal("explicit empty admin password changed the stored admin password")
		}

		shortWrite := doJSONRequest(t, handler, http.MethodPut, "/admin/api/settings", map[string]any{
			"currentPassword": "secret",
			"adminPassword":   "short",
		}, adminToken)
		if shortWrite.Code != http.StatusBadRequest {
			t.Fatalf("short admin password update status = %d, want 400; body=%s", shortWrite.Code, shortWrite.Body.String())
		}
		if afterHash := app.ConfigStore.Snapshot().Admin.Password; afterHash != beforeHash {
			t.Fatal("rejected short admin password changed the stored admin password")
		}

		if rr := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{"Username": "admin", "Pw": "secret"}, ""); rr.Code != http.StatusOK {
			t.Fatalf("original admin password no longer authenticates: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{"Username": "admin", "Pw": ""}, ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("empty admin password authenticated: status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestPhase2IAdminUserEditDoesNotExposeStoredCredentialMaterial(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		user, err := app.UserStore.Create("phase2i-secret-fields", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		adminToken := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("admin users status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		for _, forbidden := range []string{"password_hash", "passwordHash", "password_secret", "passwordSecret", user.PasswordHash, user.PasswordSecret} {
			if forbidden != "" && strings.Contains(body, forbidden) {
				t.Fatalf("admin edit response exposed stored credential material %q: %s", forbidden, body)
			}
		}
		var rows []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode admin users: %v", err)
		}
		if len(rows) != 1 || rows[0]["password"] != "password123" {
			t.Fatalf("admin edit response did not return the allowed plaintext edit credential: %#v", rows)
		}
	})
}
