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

// These are representative Emby-compatible protocol response variants, not
// authenticated SNTP captures. The live upstream must be verified separately.
func TestPhase7ASearchHintsRecoversFromEmptyGlobalItemsViaUserItems(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	observed := map[string]url.Values{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		term := r.URL.Query().Get("SearchTerm")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "fixture-only", "User": map[string]any{"Id": "upstream-search-user"},
			})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fake-search-upstream"})
		case r.URL.Path == "/Search/Hints":
			mu.Lock()
			calls["hints:"+term]++
			mu.Unlock()
			if term == "upstream-500" {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "broken"})
			} else if term == "auth-error" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "invalid login"})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
			}
		case r.URL.Path == "/Items":
			mu.Lock()
			calls["global:"+term]++
			observed["global:"+term] = cloneValues(r.URL.Query())
			mu.Unlock()
			if term == "user-direct" || term == "user-direct-empty" || term == "global-only" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": "global-found", "Type": "Movie", "Name": "Global Result", "ProductionYear": 2026,
					"MediaSources": []any{map[string]any{"Id": "source-global", "RunTimeTicks": float64(123456)}},
				}}})
			} else if term == "global-401" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "not authorized"})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			}
		case r.URL.Path == "/Users/upstream-search-user/Items":
			mu.Lock()
			calls["user:"+term]++
			observed["user:"+term] = cloneValues(r.URL.Query())
			mu.Unlock()
			if term == "user-direct" {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"Message": "unsupported"})
			} else if term == "user-direct-empty" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": "spiderman-001", "Type": "Movie", "Name": "蜘蛛侠：崭新之日",
					"ProductionYear": 2026,
					"MediaSources":   []any{map[string]any{"Id": "version-001", "RunTimeTicks": float64(99999)}},
				}}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		hints := func(term, extra string) (int, []map[string]any) {
			rr := phase2SNTPAdminRequest(t, app, http.MethodGet,
				"/Search/Hints?SearchTerm="+url.QueryEscape(term)+extra, "")
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			return rr.Code, asItems(map[string]any{"Items": body["SearchHints"]})
		}
		t.Run("empty hints and empty global items still search the authorized user's items", func(t *testing.T) {
			status, items := hints("蜘蛛侠", "&Limit=6&StartIndex=0")
			if status != http.StatusOK || len(items) != 1 {
				t.Fatalf("must recover 1 movie from upstream user-scoped Items: http=%d results=%d", status, len(items))
			}
			id, _ := items[0]["Id"].(string)
			if id == "" || id == "spiderman-001" || items[0]["ItemId"] != id {
				t.Fatalf("search result must be virtual: Id=%q ItemId=%v", id, items[0]["ItemId"])
			}
			mu.Lock()
			global, user := calls["global:蜘蛛侠"], calls["user:蜘蛛侠"]
			q := observed["user:蜘蛛侠"]
			mu.Unlock()
			if global != 1 || user != 1 || q.Get("SearchTerm") != "蜘蛛侠" ||
				q.Get("UserId") != "" || q.Get("Recursive") != "true" ||
				q.Get("Limit") != "6" || q.Get("StartIndex") != "0" {
				t.Fatalf("bad search request bounds/scope global=%d user=%d limit=%q", global, user, q.Get("Limit"))
			}
		})
		getUserItems := func(term string) (int, []map[string]any) {
			req := httptest.NewRequest(http.MethodGet,
				"/Users/proxy-fixture-user/Items?SearchTerm="+url.QueryEscape(term)+"&Recursive=true&Limit=5", nil)
			req.SetPathValue("userId", "proxy-fixture-user")
			reqCtx := &RequestContext{Headers: req.Header.Clone(),
				ProxyUser: &tokenInfo{UserID: "proxy-fixture-user", Role: "admin"}}
			req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
			rr := httptest.NewRecorder()
			app.handleUserItems(rr, req)
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			return rr.Code, asItems(body)
		}
		t.Run("client user items endpoint 404 recovers through global items", func(t *testing.T) {
			status, items := getUserItems("user-direct")
			if status != http.StatusOK || len(items) != 1 {
				t.Fatalf("user-scoped search 404 must recover via global Items: code=%d items=%d", status, len(items))
			}
		})
		t.Run("client user items endpoint empty recovers through global items", func(t *testing.T) {
			// A direct user-items search can be empty while the global
			// endpoint still holds a compatible result.
			status, items := getUserItems("user-direct-empty")
			if status != http.StatusOK || len(items) != 1 {
				t.Fatalf("user-scoped search 200 empty should try global Items: code=%d items=%d", status, len(items))
			}
		})
		t.Run("client root Items search uses authorized virtualized search", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/Items?SearchTerm="+url.QueryEscape("蜘蛛侠")+"&Limit=5", nil)
			reqCtx := &RequestContext{Headers: req.Header.Clone(),
				ProxyUser: &tokenInfo{UserID: "proxy-fixture-user", Role: "admin"}}
			req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
			rr := httptest.NewRecorder()
			app.handleItemsCollection(rr, req)
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			rows := asItems(body)
			if rr.Code != http.StatusOK || len(rows) != 1 {
				t.Fatalf("root search should recover and virtualize upstream result: http=%d rows=%d", rr.Code, len(rows))
			}
			id, _ := rows[0]["Id"].(string)
			if id == "" || id == "spiderman-001" || app.IDStore.ResolveVirtualID(id) == nil {
				t.Fatalf("root search leaked raw upstream identity, id=%q", id)
			}
		})
		t.Run("blank search never probes a second endpoint", func(t *testing.T) {
			status, items := hints("", "")
			mu.Lock()
			global, user := calls["global:"], calls["user:"]
			mu.Unlock()
			if status != 200 || len(items) != 0 || global != 0 || user != 0 {
				t.Fatalf("blank SearchTerm must not trigger search: code=%d hits=%d global=%d user=%d", status, len(items), global, user)
			}
		})
		// Authentication-error cases come last: EIO may start an upstream
		// recovery login after a 401, and that must not poison normal search tests.
		t.Run("one upstream returns 401: do not switch identity to fetch results", func(t *testing.T) {
			status, items := hints("global-401", "")
			mu.Lock()
			user := calls["user:global-401"]
			mu.Unlock()
			if status != http.StatusOK || len(items) != 0 || user != 0 {
				t.Fatalf("auth error must not permit second endpoint: status=%d hits=%d extra=%d", status, len(items), user)
			}
		})
		for _, term := range []string{"auth-error", "upstream-500"} {
			t.Run(term+" is never bypassed", func(t *testing.T) {
				status, items := hints(term, "")
				mu.Lock()
				global, user := calls["global:"+term], calls["user:"+term]
				mu.Unlock()
				if status != http.StatusOK || len(items) != 0 || global != 0 || user != 0 {
					t.Fatalf("no auth/server bypass: status=%d hits=%d global=%d user=%d", status, len(items), global, user)
				}
			})
		}

	})
}
