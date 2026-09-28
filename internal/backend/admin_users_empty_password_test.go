package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminUsersEmptyPasswordAPIContract(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")

		create := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username": "empty-api-user",
			"password": "",
		}, adminToken)
		if create.Code != http.StatusCreated {
			t.Fatalf("create empty-password user status = %d, body=%s", create.Code, create.Body.String())
		}
		if strings.Contains(create.Body.String(), `"password"`) {
			t.Fatalf("create response echoed password field: %s", create.Body.String())
		}

		var created map[string]any
		if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		userID, _ := created["id"].(string)
		if userID == "" {
			t.Fatalf("create response missing id: %#v", created)
		}

		list := doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if list.Code != http.StatusOK {
			t.Fatalf("list users status = %d, body=%s", list.Code, list.Body.String())
		}
		if got := list.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
		password := adminListedUserPassword(t, list, userID)
		if password != "" {
			t.Fatalf("listed password = %q, want empty", password)
		}

		// Omitted password is a partial update and must preserve the current credential.
		username := "empty-api-user-renamed"
		omit := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+userID, map[string]any{
			"username": username,
		}, adminToken)
		if omit.Code != http.StatusOK {
			t.Fatalf("omitted-password update status = %d, body=%s", omit.Code, omit.Body.String())
		}
		list = doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if got := adminListedUserPassword(t, list, userID); got != "" {
			t.Fatalf("omitted password changed credential to %q", got)
		}

		nonEmpty := "password123"
		set := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+userID, map[string]any{
			"password": nonEmpty,
		}, adminToken)
		if set.Code != http.StatusOK {
			t.Fatalf("set nonempty password status = %d, body=%s", set.Code, set.Body.String())
		}
		if strings.Contains(set.Body.String(), nonEmpty) || strings.Contains(set.Body.String(), `"password"`) {
			t.Fatalf("update response echoed password: %s", set.Body.String())
		}
		list = doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if got := adminListedUserPassword(t, list, userID); got != nonEmpty {
			t.Fatalf("listed password after nonempty update = %q, want %q", got, nonEmpty)
		}

		clear := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+userID, map[string]any{
			"password": "",
		}, adminToken)
		if clear.Code != http.StatusOK {
			t.Fatalf("clear password status = %d, body=%s", clear.Code, clear.Body.String())
		}
		list = doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if got := adminListedUserPassword(t, list, userID); got != "" {
			t.Fatalf("listed password after clear = %q, want empty", got)
		}
	})
}

func TestAdminUsersPasswordLengthAllowsOnlyEmptyOrEightTo128(t *testing.T) {
	withTempApp(t, func(_ *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")

		shortCreate := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username": "short-create",
			"password": "short",
		}, adminToken)
		if shortCreate.Code != http.StatusBadRequest {
			t.Fatalf("short nonempty create status = %d, want 400; body=%s", shortCreate.Code, shortCreate.Body.String())
		}

		create := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username": "length-user",
			"password": "12345678",
		}, adminToken)
		if create.Code != http.StatusCreated {
			t.Fatalf("valid create status = %d, body=%s", create.Code, create.Body.String())
		}
		var payload map[string]any
		_ = json.Unmarshal(create.Body.Bytes(), &payload)
		userID, _ := payload["id"].(string)

		shortUpdate := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+userID, map[string]any{
			"password": "short",
		}, adminToken)
		if shortUpdate.Code != http.StatusBadRequest {
			t.Fatalf("short nonempty update status = %d, want 400; body=%s", shortUpdate.Code, shortUpdate.Body.String())
		}

		emptyUpdate := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+userID, map[string]any{
			"password": "",
		}, adminToken)
		if emptyUpdate.Code != http.StatusOK {
			t.Fatalf("empty update status = %d, body=%s", emptyUpdate.Code, emptyUpdate.Body.String())
		}
	})
}

func TestAdminUsersListFailsClosedOnPasswordDecryptError(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		user, err := app.UserStore.Create("corrupt-secret", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}

		app.UserStore.mu.Lock()
		app.UserStore.users[user.ID].PasswordSecret = "v1:not-valid-base64"
		app.UserStore.mu.Unlock()

		adminToken := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("corrupt password secret list status = %d, want 500; body=%s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store even on decrypt failure", got)
		}
		if strings.Contains(rr.Body.String(), "not-valid-base64") {
			t.Fatalf("decrypt error leaked encrypted credential material: %s", rr.Body.String())
		}
	})
}

func adminListedUserPassword(t *testing.T, rr *httptest.ResponseRecorder, userID string) string {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("list users status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var users []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users list: %v", err)
	}
	for _, user := range users {
		if user["id"] != userID {
			continue
		}
		password, ok := user["password"].(string)
		if !ok {
			t.Fatalf("user %s password field missing or non-string: %#v", userID, user["password"])
		}
		return password
	}
	t.Fatalf("user %s missing from admin users list", userID)
	return ""
}
