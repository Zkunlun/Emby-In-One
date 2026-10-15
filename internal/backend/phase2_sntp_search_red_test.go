package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// A server may serve authorized item search while not implementing SearchHints.
// This is a compatibility fixture, not a claim about SNTP's authenticated API.
func TestPhase2SNTPSearchHintsNotFoundFallsBackToItems(t *testing.T) {
	var hints atomic.Int32
	var itemSearches atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "fixture-upstream-only",
				"User":        map[string]any{"Id": "sntp-test-user"},
			})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture-sntp"})
		case r.URL.Path == "/Search/Hints":
			hints.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"Message": "The requested resource was not found."})
		case r.URL.Path == "/Items" || r.URL.Path == "/Users/sntp-test-user/Items":
			itemSearches.Add(1)
			if r.URL.Query().Get("SearchTerm") != "SNTP Film" {
				t.Errorf("SearchTerm not forwarded: %q", r.URL.Query().Get("SearchTerm"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []any{map[string]any{
					"Id": "sntp-film-001", "Type": "Movie", "Name": "SNTP Film",
					"ProductionYear": 2026,
					"MediaSources":   []any{map[string]any{"Id": "film-source", "RunTimeTicks": float64(1000)}},
				}},
				"TotalRecordCount": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		rr := phase2SNTPAdminRequest(t, app, http.MethodGet,
			"/Search/Hints?SearchTerm="+url.QueryEscape("SNTP Film"), "")
		if rr.Code != http.StatusOK {
			t.Fatalf("search status=%d, body=%s", rr.Code, rr.Body.String())
		}
		var payload struct {
			SearchHints      []map[string]any `json:"SearchHints"`
			TotalRecordCount int              `json:"TotalRecordCount"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.SearchHints) != 1 || payload.TotalRecordCount != 1 {
			t.Fatalf("SearchHints=404 with Items search available must preserve one hit: got %d, total %d",
				len(payload.SearchHints), payload.TotalRecordCount)
		}
		hint := payload.SearchHints[0]
		id, _ := hint["Id"].(string)
		itemID, _ := hint["ItemId"].(string)
		if id == "" || id == "sntp-film-001" || itemID != id {
			t.Fatalf("fallback must rewrite both Id and ItemId to the same virtual ID; Id=%q ItemId=%q", id, itemID)
		}
		if hints.Load() == 0 || itemSearches.Load() == 0 {
			t.Fatalf("expected SearchHints attempt and /Items fallback, got hintCalls=%d itemCalls=%d",
				hints.Load(), itemSearches.Load())
		}
	})
}

// Execute the actual HTTP handlers under a synthetic administrator scope.
// This avoids scrypt login overhead in unrelated test setup while still
// exercising the upstream request and merge authorization paths.
func phase2SNTPAdminRequest(t *testing.T, app *App, method, target, itemID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if itemID != "" {
		req.SetPathValue("itemId", itemID)
	}
	request := &RequestContext{
		Headers:   req.Header.Clone(),
		ProxyUser: &tokenInfo{UserID: "phase2-fixture-admin", Role: "admin"},
	}
	req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, request))
	rr := httptest.NewRecorder()
	if itemID == "" {
		app.handleSearchHints(rr, req)
	} else {
		app.handleMergeItemDetail(rr, req, false)
	}
	return rr
}

func phase2SNTPFixtureConfig(upstreamURL string) string {
	return fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"SNTP Test Fixture\"\n    url: %q\n    username: \"fixture\"\n    password: \"fixture\"\n", upstreamURL)
}
