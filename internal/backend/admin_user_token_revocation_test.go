package backend

import (
	"net/http"
	"testing"
)

func TestAdminUserPasswordWriteRevokesTokens(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")
		user, err := app.UserStore.Create("token-user", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}

		issueToken := func() string {
			t.Helper()
			current := app.UserStore.Get(user.ID)
			if current == nil {
				t.Fatal("user disappeared")
			}
			hasPassword, err := app.UserStore.hasPassword(current)
			if err != nil {
				t.Fatalf("resolve user password state: %v", err)
			}
			_, token, err := app.Auth.AuthenticateUser(current, hasPassword)
			if err != nil {
				t.Fatalf("issue user token: %v", err)
			}
			if !app.Auth.HasIssuedToken(token) {
				t.Fatalf("issued token %q was not stored", token)
			}
			return token
		}

		assertTokenState := func(token string, want bool, context string) {
			t.Helper()
			if got := app.Auth.HasIssuedToken(token); got != want {
				t.Fatalf("%s: token present = %v, want %v", context, got, want)
			}
		}

		// A partial update with no password field must preserve existing sessions.
		token := issueToken()
		rr := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"username": "token-user-renamed",
		}, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("omitted password update status = %d, body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, true, "omitted password")

		// A rejected password write is not a successful write and must not revoke.
		rr = doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"password": "short",
		}, adminToken)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid password update status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, true, "rejected password write")

		// Rewriting the same non-empty password is still an explicit credential write.
		rr = doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"password": "password123",
		}, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("same password update status = %d, body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, false, "same-value password write")

		// Clearing a non-empty password revokes every token for that user.
		token = issueToken()
		rr = doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"password": "",
		}, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("clear password status = %d, body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, false, "nonempty-to-empty password write")

		// Empty-to-empty is also an explicit write and must revoke.
		token = issueToken()
		rr = doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"password": "",
		}, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("empty-to-empty password status = %d, body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, false, "empty-to-empty password write")

		// Non-password updates keep sessions unless the account is explicitly disabled.
		token = issueToken()
		enabled := true
		rr = doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"enabled": enabled,
		}, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("enabled=true update status = %d, body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, true, "enabled=true without password")

		rr = doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, map[string]any{
			"enabled": false,
		}, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("disable user status = %d, body=%s", rr.Code, rr.Body.String())
		}
		assertTokenState(token, false, "disable user")
	})
}
