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

// Phase 2 only changes /Users/{id}/Views. These contract tests protect normal
// Emby clients while the SenPlayer-specific failure tests turn green.

func TestPhase2ViewsPreservesConfiguredOrderAndDistinctIDs(t *testing.T) {
	a := phase1SenPlayerViewsSource(t, "ok")
	b := phase1SenPlayerViewsSource(t, "ok")
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK {
			t.Fatalf("Views status %d; body=%s", rr.Code, rr.Body.String())
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 2 {
			t.Fatalf("two independent source libraries expected, got %d", len(items))
		}
		for i, suffix := range []string{" (A)", " (B)"} {
			name, _ := items[i]["Name"].(string)
			if !strings.HasSuffix(name, suffix) {
				t.Fatalf("library %d name %q does not preserve configured source order", i, name)
			}
		}
		idA, _ := items[0]["Id"].(string)
		idB, _ := items[1]["Id"].(string)
		if idA == "" || idB == "" || idA == idB || idA == "fixture-library" || idB == "fixture-library" {
			t.Fatalf("source-specific virtual IDs lost or collided")
		}
	})
}

func TestPhase2ViewsBlockedFirstSourceAllowsSecondSource(t *testing.T) {
	slow := phase1SenPlayerViewsSource(t, "blocked")
	fast := phase1SenPlayerViewsSource(t, "ok")
	withTempAppConfig(t, dualUpstreamConfig(slow.URL, fast.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), ctx)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("fast second source was blocked for %v", elapsed)
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("second fast source status=%d body=%s", rr.Code, rr.Body.String())
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 1 || !strings.HasSuffix(items[0]["Name"].(string), " (B)") {
			t.Fatalf("unexpected results for slow A and fast B: %+v", items)
		}
	})
}

func TestPhase2ViewsNoSuccessfulSourceExpiresWithGlobalTimeout(t *testing.T) {
	a := phase1SenPlayerViewsSource(t, "blocked")
	b := phase1SenPlayerViewsSource(t, "blocked")
	// Existing config's global timeout is 15 seconds; make it short in this
	// isolated fixture so this test can exercise the deadline deterministically.
	config := strings.Replace(dualUpstreamConfig(a.URL, b.URL), "global: 15000", "global: 120", 1)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), ctx)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Views exceeded bounded wait: %v", elapsed)
		}
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("all blocked sources returned status=%d, expected 502: %s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "synthetic upstream") {
			t.Fatal("upstream error leaked to client")
		}
	})
}

func TestPhase2ViewsZeroOnlineSourcesReturnsEmptySuccess(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK || len(phase1SenPlayerViewsItems(t, rr)) != 0 {
			t.Fatalf("no online authorized sources must be a legitimate empty result; status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

// A successful empty upstream must not start the short grace timer while a
// different authorized upstream is still capable of returning real libraries.
// Otherwise a fast-but-empty source can reproduce SenPlayer's blank home.
func TestPhase2ViewsEmptyFirstWaitsForSlowerPopulatedSource(t *testing.T) {
	empty := phase1SenPlayerViewsSource(t, "empty")
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "synthetic-delayed-token",
				"User":        map[string]any{"Id": "fixture-delayed-user"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Views"):
			select {
			case <-time.After(350 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{{
					"Id": "delayed-library", "Name": "Late Movies", "Type": "CollectionFolder",
				}},
				"TotalRecordCount": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(delayed.Close)
	withTempAppConfig(t, dualUpstreamConfig(empty.URL, delayed.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK {
			t.Fatalf("Views status=%d body=%s", rr.Code, rr.Body.String())
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 1 || !strings.Contains(items[0]["Name"].(string), "Late Movies") {
			t.Fatalf("fast empty source prematurely suppressed an available library: %+v", items)
		}
	})
}

func TestPhase2ViewsHiddenFastDoesNotSuppressVisibleSlow(t *testing.T) {
	fast := phase1SenPlayerViewsSource(t, "ok")
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "synthetic-delayed-token",
				"User":        map[string]any{"Id": "fixture-delayed-user"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Views"):
			select {
			case <-time.After(350 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{{
					"Id": "delayed-library", "Name": "Visible Later", "Type": "CollectionFolder",
				}},
				"TotalRecordCount": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(delayed.Close)
	withTempAppConfig(t, dualUpstreamConfig(fast.URL, delayed.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		if err := app.HiddenLibraries.SetServerHidden(adminVisibilityUserID, app.Upstream.Clients()[0].ID,
			[]string{"fixture-library"}); err != nil {
			t.Fatal(err)
		}
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK {
			t.Fatalf("Views status=%d body=%s", rr.Code, rr.Body.String())
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 1 || !strings.Contains(items[0]["Name"].(string), "Visible Later") {
			t.Fatalf("hidden fast source prematurely suppressed a visible slower library: %+v", items)
		}
	})
}
