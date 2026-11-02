package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Phase 1 diagnostic contracts are derived from SenPlayer/1.0.0's real upstream
// request sequence. These fixtures never contact a real Emby server or DMIT.
func phase1SenPlayerViewsSource(t *testing.T, behavior string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "synthetic-upstream-token",
				"User":        map[string]any{"Id": "fixture-upstream-user"},
			})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Views") {
			// A real upstream may never complete its library request while its
			// other API endpoints, including authentication, remain healthy.
			switch behavior {
			case "error":
				http.Error(w, "synthetic upstream error", http.StatusBadGateway)
				return
			case "blocked":
				<-r.Context().Done()
				return
			case "empty":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Items": []any{}, "TotalRecordCount": 0,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{{
					"Id": "fixture-library", "Name": "Movies", "Type": "CollectionFolder",
					"CollectionType": "movies", "IsFolder": true,
				}},
				"TotalRecordCount": 1,
			})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "Synthetic source"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func phase1SenPlayerViewsRequest(handler http.Handler, token, userID string, ctx context.Context) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/emby/Users/"+userID+"/Views?UserId="+userID+
			"&StartIndex=0&Limit=100&Fields=PrimaryImageAspectRatio&Recursive=true", nil)
	req.Header.Set("User-Agent", "SenPlayer/1.0.0")
	req.Header.Set("X-Emby-Token", token)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func phase1SenPlayerViewsItems(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var result struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid Views JSON: %v", err)
	}
	return result.Items
}

// RED on v1.4.15: when every authorized upstream fails, the current handler
// returns HTTP 200 with an empty Items array, indistinguishable from no libraries.
func TestPhase1SenPlayerViewsAllFailedMustNotLookLikeEmptySuccess(t *testing.T) {
	broken := phase1SenPlayerViewsSource(t, "error")
	withTempAppConfig(t, singleUpstreamConfig(broken.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("all authorized upstream Views failed: status=%d, want 502, body=%s", rr.Code, rr.Body.String())
		}
	})
}

// The successful library must remain available when a second upstream fails.
func TestPhase1SenPlayerViewsPartialFailureRetainsSuccessfulLibrary(t *testing.T) {
	ok := phase1SenPlayerViewsSource(t, "ok")
	failed := phase1SenPlayerViewsSource(t, "error")
	withTempAppConfig(t, dualUpstreamConfig(ok.URL, failed.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK {
			t.Fatalf("partial Views failure: status=%d, body=%s", rr.Code, rr.Body.String())
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 1 || !strings.Contains(items[0]["Name"].(string), "Movies") {
			t.Fatalf("expected only the successful upstream's library; got %v", items)
		}
	})
}

// RED on v1.4.15: fanOutClients waits for *all* upstreams even when the
// fast upstream has completed. This simulated client cancels after 500ms.
func TestPhase1SenPlayerViewsSlowSourceMustNotBlockFastSource(t *testing.T) {
	fast := phase1SenPlayerViewsSource(t, "ok")
	slow := phase1SenPlayerViewsSource(t, "blocked")
	withTempAppConfig(t, dualUpstreamConfig(fast.URL, slow.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), ctx)
		elapsed := time.Since(start)
		if elapsed >= 400*time.Millisecond {
			t.Fatalf("Views waited %v for slow source instead of delivering fast results before client deadline", elapsed)
		}
		if rr.Code != http.StatusOK || len(phase1SenPlayerViewsItems(t, rr)) != 1 {
			t.Fatalf("expected fast source's one library: status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestPhase1SenPlayerViewsGenuinelyEmptyIsValid(t *testing.T) {
	empty := phase1SenPlayerViewsSource(t, "empty")
	withTempAppConfig(t, singleUpstreamConfig(empty.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK || len(phase1SenPlayerViewsItems(t, rr)) != 0 {
			t.Fatalf("a genuine empty successful upstream must remain HTTP 200: status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestPhase1SenPlayerViewsAnonymousCannotListLibraries(t *testing.T) {
	source := phase1SenPlayerViewsSource(t, "ok")
	withTempAppConfig(t, singleUpstreamConfig(source.URL), func(app *App, handler http.Handler) {
		rr := phase1SenPlayerViewsRequest(handler, "", app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized Views: status=%d, want 401", rr.Code)
		}
	})
}

func TestPhase1SenPlayerCountsMissingSnapshotReturnsUnavailable(t *testing.T) {
	app, _ := countsTestApp(t)
	app.mediaCounts.cache.states["b"].hasSnapshot = false
	rr := countsTestRequest(app.Handler(), http.MethodGet,
		"/emby/Items/Counts?UserId=alice", "fixture-local-token")
	countsTestAssertResponse(t, rr, http.StatusServiceUnavailable, "COUNTS_UNAVAILABLE")
}
