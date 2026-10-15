package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPhase3SearchHintsUnsupportedStatusOnly(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusBadRequest, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	} {
		t.Run(fmt.Sprintf("HTTP_%d", tc.status), func(t *testing.T) {
			err := &upstreamHTTPStatusError{
				Method: http.MethodGet, Path: "/Search/Hints",
				StatusCode: tc.status, Status: fmt.Sprintf("%d fixture", tc.status),
			}
			if got := searchHintsEndpointUnsupported(err); got != tc.want {
				t.Fatalf("status %d: unsupported=%v, want %v", tc.status, got, tc.want)
			}
		})
	}
	notSearch := &upstreamHTTPStatusError{Method: http.MethodGet, Path: "/Items", StatusCode: http.StatusNotFound}
	if searchHintsEndpointUnsupported(notSearch) {
		t.Fatal("404 from a different endpoint must not trigger search fallback")
	}
	if searchHintsEndpointUnsupported(errors.New("upstream GET /Search/Hints failed: 404")) {
		t.Fatal("raw error string must not be treated as a trustworthy HTTP status")
	}
	wrapped := fmt.Errorf("request context: %w", &upstreamHTTPStatusError{Path: "/Search/Hints", StatusCode: http.StatusNotFound})
	if !searchHintsEndpointUnsupported(wrapped) {
		t.Fatal("wrapped typed status must still be recognized")
	}
}

// One regular user's grants must be applied before requesting a fallback.
// Test-only in-memory principal avoids password hashing but uses EIO's actual
// allowedClients/mediaAccessScopeLocked authorization gates.
func TestPhase3SearchFallbackHonorsSingleServerGrant(t *testing.T) {
	var allowedCalls, deniedCalls atomic.Int32
	fake := func(label string, calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture-token-" + label, "User": map[string]any{"Id": "up-" + label}})
			case r.URL.Path == "/System/Info/Public":
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": label})
			case r.URL.Path == "/Search/Hints":
				calls.Add(1)
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "unsupported"})
			case r.URL.Path == "/Items":
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": label + "-item", "Type": "Movie", "Name": "grant sample",
					"MediaSources": []any{map[string]any{"Id": label + "-source", "RunTimeTicks": float64(111)}},
				}}})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a, b := fake("allowed", &allowedCalls), fake("denied", &deniedCalls)
	defer a.Close()
	defer b.Close()
	config := phase2SNTPFixtureConfig(a.URL) + fmt.Sprintf(
		"  - name: \"Unauthorized Test Upstream\"\n    url: %q\n    username: \"fixture\"\n    password: \"fixture\"\n", b.URL)
	withTempAppConfig(t, config, func(app *App, _ http.Handler) {
		clients := app.Upstream.Clients()
		if len(clients) != 2 {
			t.Fatalf("expected two upstreams, got %d", len(clients))
		}
		id := "fixture-user-single-grant"
		app.UserStore.mu.Lock()
		app.UserStore.users[id] = &User{ID: id, Username: "test", Enabled: true, AllowedServers: []string{clients[0].ID}}
		app.UserStore.mu.Unlock()
		req := httptest.NewRequest(http.MethodGet, "/Search/Hints?SearchTerm=grant", nil)
		reqCtx := &RequestContext{Headers: req.Header.Clone(), ProxyUser: &tokenInfo{
			UserID: id, Username: "test", Role: "user", AllowedServers: []string{clients[0].ID},
		}}
		req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
		rr := httptest.NewRecorder()
		app.handleSearchHints(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("search status=%d: %s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		hits := asItems(map[string]any{"Items": payload["SearchHints"]})
		if len(hits) != 1 {
			t.Fatalf("single-grant user must see only allowed upstream, got %d", len(hits))
		}
		virtual, _ := hits[0]["Id"].(string)
		owner := app.IDStore.ResolveVirtualID(virtual)
		if owner == nil || owner.ServerID != clients[0].ID || virtual == "" {
			t.Fatalf("search result not bound to authorized server, id=%q", virtual)
		}
		if allowedCalls.Load() == 0 || deniedCalls.Load() != 0 {
			t.Fatalf("upstream calls must stay within authorization: allowed=%d denied=%d", allowedCalls.Load(), deniedCalls.Load())
		}
	})
}

// The server simulates protocol variants, not an authenticated SNTP capture.
// Keep the fixture one-server and reuse one app, so the assertions don't depend
// on repeated SQLite setup or costly password-hash logins.
func TestPhase3SNTPSearchCompatibilityBoundaries(t *testing.T) {
	var mu sync.Mutex
	hintsByTerm := map[string]map[string]any{
		"only-id":     {"Id": "original-only-id", "Type": "Movie", "Name": "Only ID", "ProductionYear": 2024},
		"only-itemid": {"ItemId": "original-only-itemid", "Type": "Movie", "Name": "Only ItemId", "ProductionYear": 2024},
		"both":        {"Id": "original-both", "ItemId": "original-both", "Type": "Movie", "Name": "Both Fields", "ProductionYear": 2024},
		"mismatch":    {"Id": "original-preferred", "ItemId": "other-original", "Type": "Movie", "Name": "Mismatched Fields", "ProductionYear": 2024},
		"missing":     {"Type": "Movie", "Name": "Missing Identity", "ProductionYear": 2024},
	}
	fallbackCalls := map[string]int{}
	rawFallback := map[string]url.Values{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		term := r.URL.Query().Get("SearchTerm")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fake-upstream-token", "User": map[string]any{"Id": "user-fixture"}})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture"})
		case r.URL.Path == "/Search/Hints":
			if term == "unsupported" || term == "emptyterm" {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "not found"})
				return
			}
			if term == "method-not-allowed" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "not supported"})
				return
			}
			if term == "broken" {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "temporary failure"})
				return
			}
			if term == "empty" {
				_ = json.NewEncoder(w).Encode(map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
				return
			}
			if term == "malformed" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Unrecognized": []any{}, "TotalRecordCount": 0})
				return
			}
			if hint := hintsByTerm[term]; hint != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"SearchHints": []any{hint}, "TotalRecordCount": 1})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
		case r.URL.Path == "/Users/user-fixture/Items" || r.URL.Path == "/Items":
			// Existing EIO detail hydration also calls /Items with Ids rather
			// than SearchTerm; do not miscount that as a search fallback.
			if term == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
				return
			}
			mu.Lock()
			fallbackCalls[term]++
			rawFallback[term] = r.URL.Query()
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
				"Id": "fallback-item", "Type": "Movie", "Name": "Fallback Movie", "ProductionYear": 2024,
				"MediaSources": []any{map[string]any{"Id": "fallback-src", "RunTimeTicks": float64(9999)}},
			}}, "TotalRecordCount": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		get := func(term, extra string) (int, []map[string]any) {
			target := "/Search/Hints?SearchTerm=" + url.QueryEscape(term) + extra
			rr := phase2SNTPAdminRequest(t, app, http.MethodGet, target, "")
			var payload struct {
				SearchHints      []map[string]any `json:"SearchHints"`
				TotalRecordCount int              `json:"TotalRecordCount"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatalf("bad json: %v", err)
			}
			if payload.TotalRecordCount != len(payload.SearchHints) {
				t.Errorf("%s total=%d length=%d", term, payload.TotalRecordCount, len(payload.SearchHints))
			}
			return rr.Code, payload.SearchHints
		}
		for _, term := range []string{"only-id", "only-itemid", "both", "mismatch"} {
			t.Run(term, func(t *testing.T) {
				status, items := get(term, "")
				if status != 200 || len(items) != 1 {
					t.Fatalf("status=%d count=%d", status, len(items))
				}
				id, _ := items[0]["Id"].(string)
				alias, _ := items[0]["ItemId"].(string)
				if id == "" || id != alias || strings.HasPrefix(id, "original") || alias == "other-original" {
					t.Fatalf("search result must have identical virtual Id and ItemId, got Id=%q ItemId=%q", id, alias)
				}
				mu.Lock()
				count := fallbackCalls[term]
				mu.Unlock()
				if count != 0 {
					t.Fatalf("successful hints must not invoke /Items fallback, got %d", count)
				}
			})
		}
		t.Run("missing identity", func(t *testing.T) {
			status, items := get("missing", "")
			if status != 200 || len(items) != 0 {
				t.Fatalf("identity-less hint should not be returned, status=%d items=%d", status, len(items))
			}
		})
		t.Run("404 fallback authorized and bounded", func(t *testing.T) {
			status, items := get("unsupported", "&Limit=3&StartIndex=2")
			if status != 200 || len(items) != 1 {
				t.Fatalf("status=%d, count=%d", status, len(items))
			}
			id, _ := items[0]["Id"].(string)
			alias, _ := items[0]["ItemId"].(string)
			if id == "" || id != alias || id == "fallback-item" {
				t.Fatalf("raw source id leaked: Id=%q ItemId=%q", id, alias)
			}
			mu.Lock()
			count := fallbackCalls["unsupported"]
			q := rawFallback["unsupported"]
			mu.Unlock()
			if count != 1 || q.Get("SearchTerm") != "unsupported" || q.Get("StartIndex") != "2" || q.Get("Limit") != "3" || q.Get("Recursive") != "true" || q.Get("UserId") != "user-fixture" {
				t.Fatalf("fallback should preserve search paging, user scope and recursion: calls=%d, query=%v", count, q)
			}
		})
		t.Run("405 fallback", func(t *testing.T) {
			status, items := get("method-not-allowed", "")
			mu.Lock()
			count := fallbackCalls["method-not-allowed"]
			q := rawFallback["method-not-allowed"]
			mu.Unlock()
			if status != 200 || len(items) != 1 || count != 1 || q.Get("Limit") == "" {
				t.Fatalf("405 fallback failure: status=%d items=%d calls=%d limit=%q", status, len(items), count, q.Get("Limit"))
			}
		})
		t.Run("500 does not fallback", func(t *testing.T) {
			status, items := get("broken", "")
			mu.Lock()
			count := fallbackCalls["broken"]
			mu.Unlock()
			if status != 200 || len(items) != 0 || count != 0 {
				t.Fatalf("500 must not trigger extra search requests: status=%d items=%d calls=%d", status, len(items), count)
			}
		})
		t.Run("200 empty hints fallback to bounded item search", func(t *testing.T) {
			status, items := get("empty", "&Limit=5&StartIndex=0")
			mu.Lock()
			count := fallbackCalls["empty"]
			q := rawFallback["empty"]
			mu.Unlock()
			if status != 200 || len(items) != 1 || count != 1 ||
				q.Get("SearchTerm") != "empty" || q.Get("Limit") != "5" ||
				q.Get("StartIndex") != "0" || q.Get("Recursive") != "true" ||
				q.Get("UserId") != "user-fixture" {
				t.Fatalf("200 empty hints must perform one scoped bounded fallback: status=%d items=%d calls=%d query=%v", status, len(items), count, q)
			}
		})
		t.Run("200 unexpected structure fallback to item search", func(t *testing.T) {
			status, items := get("malformed", "")
			mu.Lock()
			count := fallbackCalls["malformed"]
			q := rawFallback["malformed"]
			mu.Unlock()
			if status != 200 || len(items) != 1 || count != 1 || q.Get("Limit") != "20" {
				t.Fatalf("200 unusable hints should request a limited fallback: status=%d items=%d calls=%d query=%v", status, len(items), count, q)
			}
		})
		t.Run("empty SearchTerm cannot enumerate entire upstream", func(t *testing.T) {
			status, items := get("", "")
			mu.Lock()
			count := fallbackCalls[""]
			mu.Unlock()
			if status != 200 || len(items) != 0 || count != 0 {
				t.Fatalf("blank search must not request all upstream items: status=%d items=%d calls=%d", status, len(items), count)
			}
		})
	})
}
