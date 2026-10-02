package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase1FAccessUpstream(t *testing.T, token, userID, itemID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": token,
				"User":        map[string]any{"Id": userID},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/"+itemID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":   itemID,
				"Name": itemID,
				"Type": "Movie",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase1FSingleConfig(url string) string {
	return fmt.Sprintf(`server:
  port: 8096
  name: "Test"
  id: "svr"
admin:
  username: "admin"
  password: "secret"
playback:
  mode: "proxy"
timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 60000
proxies: []
upstream:
  - id: "server-a"
    name: "A"
    url: %q
    username: "u1"
    password: "p1"
    maxConcurrent: 2
`, url)
}

func phase1FCreateUser(t *testing.T, handler http.Handler, username string, allowedServers []string) string {
	t.Helper()
	adminToken := loginTokenAs(t, handler, "admin", "secret")
	rr := doAuthJSON(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
		"username":       username,
		"password":       "password123",
		"allowedServers": allowedServers,
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create user %s: status=%d body=%s", username, rr.Code, rr.Body.String())
	}
	return loginTokenAs(t, handler, username, "password123")
}

func TestPhase1FRegularUserWithNoAssignedServersHasNoUpstreamAccess(t *testing.T) {
	upstream := phase1FAccessUpstream(t, "tok-a", "user-a", "item-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1FSingleConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := phase1FCreateUser(t, handler, "alice", []string{})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular user login did not produce a valid token")
		}

		ctx := &RequestContext{ProxyUser: info}
		if got := len(app.allowedClients(ctx)); got != 0 {
			t.Errorf("unassigned regular user sees %d upstream(s), want 0", got)
		}
		if app.isServerAllowed(ctx, "server-a") {
			t.Errorf("unassigned regular user unexpectedly allowed to server-a")
		}

		virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		rr := doAuthJSON(t, handler, http.MethodGet, "/Items/"+virtualItem, nil, token)
		if rr.Code != http.StatusForbidden {
			t.Errorf("unassigned regular user item access status=%d, want 403 body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestPhase1FRegularUserSeesOnlyExplicitlyAssignedServers(t *testing.T) {
	upstreamA := phase1FAccessUpstream(t, "tok-a", "user-a", "item-a")
	defer upstreamA.Close()
	upstreamB := phase1FAccessUpstream(t, "tok-b", "user-b", "item-b")
	defer upstreamB.Close()

	withTempAppConfig(t, phase1EDualPlaybackConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		token := phase1FCreateUser(t, handler, "alice", []string{"server-a"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular user login did not produce a valid token")
		}
		ctx := &RequestContext{ProxyUser: info}

		allowed := app.allowedClients(ctx)
		if len(allowed) != 1 || allowed[0].ID != "server-a" {
			t.Fatalf("explicit server authorization returned %#v, want only server-a", allowed)
		}
		if !app.isServerAllowed(ctx, "server-a") {
			t.Error("explicitly assigned server-a was denied")
		}
		if app.isServerAllowed(ctx, "server-b") {
			t.Error("unassigned server-b was allowed")
		}
	})
}

func TestPhase1FRegularUserCanBeExplicitlyAssignedMultipleServers(t *testing.T) {
	upstreamA := phase1FAccessUpstream(t, "tok-a", "user-a", "item-a")
	defer upstreamA.Close()
	upstreamB := phase1FAccessUpstream(t, "tok-b", "user-b", "item-b")
	defer upstreamB.Close()

	withTempAppConfig(t, phase1EDualPlaybackConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		token := phase1FCreateUser(t, handler, "alice", []string{"server-a", "server-b"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular user login did not produce a valid token")
		}
		ctx := &RequestContext{ProxyUser: info}

		if got := len(app.allowedClients(ctx)); got != 2 {
			t.Fatalf("explicit two-server authorization returned %d upstreams, want 2", got)
		}
		if !app.isServerAllowed(ctx, "server-a") || !app.isServerAllowed(ctx, "server-b") {
			t.Fatal("explicit multi-server authorization did not allow both servers")
		}
	})
}

func TestPhase1FAdminStillHasImplicitAccessToAllServers(t *testing.T) {
	upstreamA := phase1FAccessUpstream(t, "tok-a", "user-a", "item-a")
	defer upstreamA.Close()
	upstreamB := phase1FAccessUpstream(t, "tok-b", "user-b", "item-b")
	defer upstreamB.Close()

	withTempAppConfig(t, phase1EDualPlaybackConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		info := app.Auth.ValidateToken(adminToken)
		if info == nil || info.Role != "admin" {
			t.Fatalf("admin token invalid: %#v", info)
		}
		ctx := &RequestContext{ProxyUser: info}

		if got := len(app.allowedClients(ctx)); got != 2 {
			t.Fatalf("admin sees %d upstreams, want 2", got)
		}
		if !app.isServerAllowed(ctx, "server-a") || !app.isServerAllowed(ctx, "server-b") {
			t.Fatal("admin lost implicit all-server access")
		}
	})
}
