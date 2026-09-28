package backend

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEmptyPasswordUserStoreAndAdminAPIContract(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")

		create := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username":       "empty-user",
			"password":       "",
			"allowedServers": []string{},
		}, adminToken)
		if create.Code != http.StatusCreated {
			t.Fatalf("create empty-password user status = %d, body=%s", create.Code, create.Body.String())
		}

		if got := app.UserStore.Authenticate("empty-user", ""); got == nil {
			t.Fatal("UserStore.Authenticate rejected matching empty password")
		}
		if got := app.UserStore.Authenticate("empty-user", "wrong-password"); got != nil {
			t.Fatal("UserStore.Authenticate accepted wrong password for empty-password user")
		}

		list := doJSONRequest(t, handler, http.MethodGet, "/admin/api/users", nil, adminToken)
		if list.Code != http.StatusOK {
			t.Fatalf("list users status = %d, body=%s", list.Code, list.Body.String())
		}
		var users []map[string]any
		if err := json.Unmarshal(list.Body.Bytes(), &users); err != nil {
			t.Fatalf("unmarshal users: %v", err)
		}
		var id string
		for _, u := range users {
			if u["username"] == "empty-user" {
				id, _ = u["id"].(string)
				password, ok := u["password"].(string)
				if !ok || password != "" {
					t.Fatalf("admin users password = %#v, want explicit empty string", u["password"])
				}
			}
		}
		if id == "" {
			t.Fatal("created empty-password user missing from admin list")
		}

		for _, password := range []string{"12345678", "", "abcdefgh", ""} {
			update := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+id, map[string]any{"password": password}, adminToken)
			if update.Code != http.StatusOK {
				t.Fatalf("update password %q status = %d, body=%s", password, update.Code, update.Body.String())
			}
			if got := app.UserStore.Authenticate("empty-user", password); got == nil {
				t.Fatalf("Authenticate rejected password state %q after update", password)
			}
		}
	})
}

func TestAuthenticateByNameAcceptsEmptyPasswordUser(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		user, err := app.UserStore.Create("empty-login", "", nil)
		if err != nil {
			t.Fatalf("create empty-password user: %v", err)
		}

		rr := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
			"Username": user.Username,
			"Pw":       "",
		}, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("empty-password AuthenticateByName status = %d, body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal login response: %v", err)
		}
		if token, _ := payload["AccessToken"].(string); token == "" {
			t.Fatalf("missing AccessToken: %#v", payload)
		}
		userObject, _ := payload["User"].(map[string]any)
		if got, _ := userObject["HasPassword"].(bool); got {
			t.Fatalf("empty-password user HasPassword = true, want false")
		}
		if got, _ := userObject["HasConfiguredPassword"].(bool); got {
			t.Fatalf("empty-password user HasConfiguredPassword = true, want false")
		}
		if got, _ := userObject["EnableAutoLogin"].(bool); got {
			t.Fatalf("empty-password user EnableAutoLogin = true, want false")
		}

		wrong := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
			"Username": user.Username,
			"Pw":       "wrong-password",
		}, "")
		if wrong.Code == http.StatusOK {
			t.Fatalf("wrong password unexpectedly authenticated: %s", wrong.Body.String())
		}
	})
}

func TestUserObjectPasswordFlagsFollowActualPassword(t *testing.T) {
	withTempApp(t, func(app *App, _ http.Handler) {
		emptyUser, err := app.UserStore.Create("empty-flags", "", nil)
		if err != nil {
			t.Fatalf("create empty user: %v", err)
		}
		normalUser, err := app.UserStore.Create("normal-flags", "12345678", nil)
		if err != nil {
			t.Fatalf("create normal user: %v", err)
		}

		emptyHasPassword, err := app.UserStore.hasPassword(emptyUser)
		if err != nil {
			t.Fatalf("resolve empty user password state: %v", err)
		}
		emptyObject := app.Auth.BuildUserObjectForUser(emptyUser, emptyHasPassword)
		if emptyObject["HasPassword"] != false || emptyObject["HasConfiguredPassword"] != false {
			t.Fatalf("empty-password flags = HasPassword:%#v HasConfiguredPassword:%#v, want false/false", emptyObject["HasPassword"], emptyObject["HasConfiguredPassword"])
		}
		if emptyObject["HasConfiguredEasyPassword"] != false || emptyObject["EnableAutoLogin"] != false {
			t.Fatalf("empty-password easy/auto flags changed: %#v", emptyObject)
		}

		normalHasPassword, err := app.UserStore.hasPassword(normalUser)
		if err != nil {
			t.Fatalf("resolve normal user password state: %v", err)
		}
		normalObject := app.Auth.BuildUserObjectForUser(normalUser, normalHasPassword)
		if normalObject["HasPassword"] != true || normalObject["HasConfiguredPassword"] != true {
			t.Fatalf("nonempty-password flags = HasPassword:%#v HasConfiguredPassword:%#v, want true/true", normalObject["HasPassword"], normalObject["HasConfiguredPassword"])
		}

		adminObject := app.Auth.BuildUserObject()
		if adminObject["HasPassword"] != true || adminObject["HasConfiguredPassword"] != true {
			t.Fatalf("admin password flags changed: %#v", adminObject)
		}
	})
}
