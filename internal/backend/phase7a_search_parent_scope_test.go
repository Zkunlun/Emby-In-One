package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

func TestPhase7ARootItemsParentSearchPreservesSpecificLibraryRoute(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	parentSent := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "fixture-only",
				"User":        map[string]any{"Id": "fixture-auth-user"},
			})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture"})
		case r.URL.Path == "/Items":
			// Background detail hydration is not another search request.
			if r.URL.Query().Get("SearchTerm") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
				return
			}
			mu.Lock()
			calls++
			parentSent = r.URL.Query().Get("ParentId")
			mu.Unlock()
			if r.URL.Query().Get("ParentId") != "original-library" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []any{map[string]any{
					"Id": "original-movie", "Name": "蜘蛛侠：崭新之日", "Type": "Movie", "ProductionYear": 2026,
				}},
				"TotalRecordCount": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		clients := app.Upstream.OnlineClients()
		if len(clients) != 1 {
			t.Fatalf("fixture clients=%d, want 1", len(clients))
		}
		virtualLibrary := app.IDStore.GetOrCreateVirtualID("original-library", clients[0].ID)
		request := httptest.NewRequest(http.MethodGet,
			"/Items?SearchTerm="+url.QueryEscape("蜘蛛侠")+"&ParentId="+url.QueryEscape(virtualLibrary)+"&Limit=3", nil)
		reqCtx := &RequestContext{
			Headers:   request.Header.Clone(),
			ProxyUser: &tokenInfo{UserID: "phase7a-admin", Role: "admin"},
		}
		request = request.WithContext(context.WithValue(request.Context(), requestContextKey{}, reqCtx))
		result := httptest.NewRecorder()
		app.handleItemsCollection(result, request)
		var payload map[string]any
		if err := json.Unmarshal(result.Body.Bytes(), &payload); err != nil {
			t.Fatalf("response JSON: %v", err)
		}
		got := asItems(payload)
		mu.Lock()
		count, rawParent := calls, parentSent
		mu.Unlock()
		if result.Code != http.StatusOK || len(got) != 1 || count != 1 || rawParent != "original-library" {
			t.Fatalf("library-scoped search must use existing virtual-parent routing: status=%d count=%d calls=%d rawParent=%q",
				result.Code, len(got), count, rawParent)
		}
	})
}

func TestPhase7ASearchCandidateWindowBounds(t *testing.T) {
	for _, tc := range []struct {
		name, start, limit, want string
	}{
		{"first", "0", "5", "5"},
		{"next", "20", "5", "25"},
		{"implicit", "", "", "20"},
		{"bad", "-5", "bad", "20"},
		{"cap", "8000", "20000", "5000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := url.Values{"SearchTerm": {"蜘蛛侠"}}
			if tc.start != "" {
				original.Set("StartIndex", tc.start)
			}
			if tc.limit != "" {
				original.Set("Limit", tc.limit)
			}
			requested := cloneValues(original)
			requestBoundedSearchCandidateSet(requested, original)
			if requested.Get("StartIndex") != "0" || requested.Get("Limit") != tc.want {
				t.Fatalf("search candidates start=%q limit=%q; want start=0 limit=%s",
					requested.Get("StartIndex"), requested.Get("Limit"), tc.want)
			}
			if original.Get("StartIndex") != tc.start || original.Get("Limit") != tc.limit {
				t.Fatal("bounded query must not mutate client request")
			}
		})
	}
}
