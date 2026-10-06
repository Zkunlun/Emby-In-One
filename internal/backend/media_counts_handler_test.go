package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

func countsTestAssertResponse(t *testing.T, rr *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "private, no-store" || rr.Header().Get("ETag") != "" || rr.Header().Get("Last-Modified") != "" {
		t.Fatal("cache boundary missing")
	}
	if status != 200 {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["MovieCount"]; ok {
			t.Fatal("error leaked partial counts")
		}
		if code != "" {
			var got string
			_ = json.Unmarshal(body["code"], &got)
			if got != code {
				t.Fatalf("code %q", got)
			}
		}
	}
}
func countsTestValue(t *testing.T, rr *httptest.ResponseRecorder) mediaCounts {
	t.Helper()
	var value mediaCounts
	if err := json.Unmarshal(rr.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &fields); err != nil || len(fields) != 3 {
		t.Fatal("unexpected success fields", err)
	}
	return value
}

func TestMediaCountsClientRouteQueryAndIdentity(t *testing.T) {
	cases := []struct {
		name, target, token string
		status              int
		code                string
	}{
		{"default", "/Items/Counts", "fixture-local-token", 200, ""},
		{"prefix", "/emby/Items/Counts", "fixture-local-token", 200, ""},
		{"local alias", "/Items/Counts?uSeRiD=alice", "fixture-local-token", 200, ""},
		{"legacy alias", "/Items/Counts?UserId=legacy", "fixture-local-token", 200, ""},
		{"mixed query auth", "/Items/Counts?aPiKeY=fixture-local-token", "", 200, ""},
		{"query underscore", "/Items/Counts?API_KEY=fixture-local-token", "", 200, ""},
		{"metadata", "/Items/Counts?X-Emby-Client=real-client&User-Agent=real-agent&X-Emby-Token=metadata-only", "fixture-local-token", 200, ""},
		{"metadata not auth", "/Items/Counts?X-Emby-Token=fixture-local-token", "", 401, ""},
		{"empty user", "/Items/Counts?UserId=", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"duplicate user", "/Items/Counts?UserId=alice&userid=alice", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"duplicate auth", "/Items/Counts?api_key=fixture-local-token&ApiKey=fixture-local-token", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"malformed", "/Items/Counts?UserId=%zz", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"semicolon", "/Items/Counts?x=1;UserId=alice", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"favorite true", "/Items/Counts?IsFavorite=true", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"favorite false", "/Items/Counts?IsFavorite=false", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"favorite empty", "/Items/Counts?IsFavorite=", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"select source", "/Items/Counts?ServerId=a", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"page", "/Items/Counts?Limit=1", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"other local user", "/Items/Counts?UserId=bob", "fixture-local-token", 403, "COUNTS_USER_FORBIDDEN"},
		{"real upstream user", "/Items/Counts?UserId=real-a", "fixture-local-token", 403, "COUNTS_USER_FORBIDDEN"},
		{"missing auth", "/Items/Counts", "", 401, ""},
		{"invalid token", "/Items/Counts", "unknown-fixture", 401, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := countsTestApp(t)
			// Full Handler construction checks actual ServeMux registration conflicts.
			rr := countsTestRequest(app.Handler(), http.MethodGet, tc.target, tc.token)
			countsTestAssertResponse(t, rr, tc.status, tc.code)
			if tc.status == 200 && countsTestValue(t, rr) != (mediaCounts{30, 3, 300}) {
				t.Fatal("wrong scope/sum")
			}
		})
	}
}

func TestMediaCountsClientCurrentScopeAndReadOnly(t *testing.T) {
	app, _ := countsTestApp(t)
	app.HiddenLibraries = &HiddenLibraryStore{}
	app.HiddenLibraries.replaceUserServers("alice", map[string]map[string]struct{}{"a": {"*": {}, "fixture-library": {}}})
	handler := app.Handler()
	beforeA, beforeB := app.mediaCounts.cache.read("a"), app.mediaCounts.cache.read("b")
	for i := 0; i < 20; i++ {
		rr := countsTestRequest(handler, "GET", "/Items/Counts", "fixture-local-token")
		if rr.Code != 200 || countsTestValue(t, rr) != (mediaCounts{30, 3, 300}) {
			t.Fatal("hidden home affected total")
		}
	}
	if !reflect.DeepEqual(beforeA, app.mediaCounts.cache.read("a")) || !reflect.DeepEqual(beforeB, app.mediaCounts.cache.read("b")) || len(app.mediaCounts.wake) != 0 {
		t.Fatal("reads mutated pending/flight/cache or signaled work")
	}
	// Current durable bindings override the older request token's two-source list.
	app.watchLifecycleMu.Lock()
	app.UserStore.mu.Lock()
	app.UserStore.users["alice"].AllowedServers = []string{"a", "a"}
	app.UserStore.mu.Unlock()
	app.watchLifecycleMu.Unlock()
	rr := countsTestRequest(handler, "GET", "/Items/Counts", "fixture-local-token")
	if countsTestValue(t, rr) != (mediaCounts{10, 1, 100}) {
		t.Fatal("stale token expanded current binding")
	}
	admin := countsTestRequest(handler, "GET", "/Items/Counts", "fixture-admin-token")
	if countsTestValue(t, admin) != (mediaCounts{30, 3, 300}) {
		t.Fatal("admin did not see configured scope")
	}
	// Conversely, a token limited to a cannot gain b solely from current bindings.
	app.UserStore.mu.Lock()
	app.UserStore.users["alice"].AllowedServers = []string{"a", "b"}
	app.UserStore.mu.Unlock()
	app.Auth.mu.Lock()
	info := app.Auth.tokens["fixture-local-token"]
	info.AllowedServers = []string{"a"}
	app.Auth.tokens["fixture-local-token"] = info
	app.Auth.mu.Unlock()
	rr = countsTestRequest(handler, "GET", "/Items/Counts", "fixture-local-token")
	if countsTestValue(t, rr) != (mediaCounts{10, 1, 100}) {
		t.Fatal("current binding expanded token grant")
	}
}

func TestMediaCountsClientUnavailableZeroAndGenerations(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*App)
		status int
		code   string
		value  mediaCounts
	}{
		{"online missing", func(a *App) { a.mediaCounts.cache.states["b"].hasSnapshot = false }, 503, "COUNTS_UNAVAILABLE", mediaCounts{}},
		{"all offline no caches", func(a *App) {
			for _, c := range a.Upstream.clients {
				c.Online = false
			}
			a.mediaCounts.cache.states = map[string]*countsSourceState{}
		}, 200, "", mediaCounts{}},
		{"offline excluded", func(a *App) {
			a.Upstream.clients[1].Online = false
			a.mediaCounts.cache.states["b"].hasSnapshot = false
		}, 200, "", mediaCounts{10, 1, 100}},
		{"empty bindings", func(a *App) { a.UserStore.users["alice"].AllowedServers = nil }, 200, "", mediaCounts{}},
		{"pending", func(a *App) { a.lifecyclePending = true }, 503, "COUNTS_LIFECYCLE_PENDING", mediaCounts{}},
		{"disabled", func(a *App) { a.UserStore.users["alice"].Enabled = false }, 401, "", mediaCounts{}},
		{"deleted", func(a *App) { delete(a.UserStore.users, "alice") }, 401, "", mediaCounts{}},
		{"revision changed", func(a *App) { a.UserStore.users["alice"].AuthRevision++ }, 401, "", mediaCounts{}},
		{"revoked", func(a *App) { delete(a.Auth.tokens, "fixture-local-token") }, 401, "", mediaCounts{}},
		{"cache generation", func(a *App) { a.mediaCounts.cache.states["a"].dataGeneration++ }, 503, "COUNTS_UNAVAILABLE", mediaCounts{}},
		{"real account changed", func(a *App) { a.Upstream.clients[0].UserID = "new-real-user" }, 503, "COUNTS_UNAVAILABLE", mediaCounts{}},
		{"account not authenticated", func(a *App) { a.Upstream.clients[0].countsLoginConfig = countsLoginBinding{} }, 503, "COUNTS_UNAVAILABLE", mediaCounts{}},
		{"proxy changed", func(a *App) {
			c := a.Upstream.clients[0]
			c.Config.ProxyID = "proxy"
			a.ConfigStore.config.Upstream[0] = c.Config
			a.ConfigStore.config.Proxies = []ProxyConfig{{ID: "proxy", URL: "http://fixture.invalid:8000"}}
		}, 503, "COUNTS_UNAVAILABLE", mediaCounts{}},
		{"invalid snapshot", func(a *App) { a.mediaCounts.cache.states["a"].snapshot.Value.MovieCount = -1 }, 503, "COUNTS_OUT_OF_RANGE", mediaCounts{}},
		{"same account token rotated", func(a *App) { a.Upstream.clients[0].AccessToken = "rotated" }, 200, "", mediaCounts{30, 3, 300}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := countsTestApp(t)
			tc.modify(app)
			rr := countsTestRequest(app.Handler(), "GET", "/Items/Counts", "fixture-local-token")
			countsTestAssertResponse(t, rr, tc.status, tc.code)
			if tc.status == 200 && countsTestValue(t, rr) != tc.value {
				t.Fatal("unexpected total")
			}
		})
	}
}

func TestMediaCountsClientHeadMethodsAndConditional(t *testing.T) {
	app, _ := countsTestApp(t)
	handler := app.Handler()
	for _, target := range []string{"/Items/Counts", "/emby/Items/Counts", "/Items/Counts?IsFavorite=false"} {
		for _, token := range []string{"fixture-local-token", ""} {
			get := countsTestRequest(handler, "GET", target, token)
			head := countsTestRequest(handler, "HEAD", target, token)
			if head.Code != get.Code || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) ||
				head.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("HEAD differs from GET validation/representation")
			}
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		for _, target := range []string{"/Items/Counts", "/emby/Items/Counts"} {
			rr := countsTestRequest(handler, method, target, "fixture-local-token")
			countsTestAssertResponse(t, rr, 405, "METHOD_NOT_ALLOWED")
			if rr.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("Allow missing")
			}
			rr = countsTestRequest(handler, method, target, "")
			countsTestAssertResponse(t, rr, 401, "")
		}
	}
	rr := countsTestRequest(handler, "OPTIONS", "/System/Info/Public", "")
	if rr.Code != 200 {
		t.Fatal("other CORS preflight changed")
	}
	req := httptest.NewRequest("GET", "/Items/Counts", nil)
	req.Header.Set("X-Emby-Token", "fixture-local-token")
	req.Header.Set("If-None-Match", "*")
	req.Header.Set("If-Modified-Since", "Mon, 05 Oct 2026 00:00:00 GMT")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	countsTestAssertResponse(t, rr, 200, "")
	if rr.Header().Get("Vary") == "" {
		t.Fatal("auth Vary missing")
	}
}

func TestMediaCountsClientRechecksInFlightIdentity(t *testing.T) {
	app, _ := countsTestApp(t)
	info := app.Auth.ValidateToken("fixture-local-token")
	request := httptest.NewRequest("GET", "/Items/Counts", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey{}, &RequestContext{ProxyUser: info, ProxyToken: "fixture-local-token", LegacyProxyUserID: "legacy"}))
	app.Auth.mu.Lock()
	delete(app.Auth.tokens, "fixture-local-token")
	app.Auth.mu.Unlock()
	rr := httptest.NewRecorder()
	app.handleItemsCounts(rr, request)
	countsTestAssertResponse(t, rr, 401, "")
	// Admin credentials remain authenticated during cleanup, but Counts is 503.
	app.lifecyclePending = true
	rr = countsTestRequest(app.Handler(), "GET", "/Items/Counts", "fixture-admin-token")
	countsTestAssertResponse(t, rr, 503, "COUNTS_LIFECYCLE_PENDING")
}
