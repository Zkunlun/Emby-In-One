package backend

import (
	"context"
	"encoding/json"
	"fmt"
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
	config := strings.Replace(dualUpstreamConfig(slow.URL, fast.URL), "global: 15000", "global: 15000\n  metadataGracePeriod: 150", 1)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
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
	config := strings.Replace(dualUpstreamConfig(empty.URL, delayed.URL), "global: 15000", "global: 15000\n  metadataGracePeriod: 150", 1)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
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
	config := strings.Replace(dualUpstreamConfig(fast.URL, delayed.URL), "global: 15000", "global: 15000\n  metadataGracePeriod: 150", 1)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
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

func phase2DelayedViewsSource(t *testing.T, delay time.Duration, libID, libName string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "synthetic-delayed-token",
				"User":        map[string]any{"Id": "fixture-delayed-user"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Views"):
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{{
					"Id": libID, "Name": libName, "Type": "CollectionFolder",
					"CollectionType": "movies", "IsFolder": true,
				}},
				"TotalRecordCount": 1,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "Synthetic source"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func tripleUpstreamConfig(url1, url2, url3 string, extraTimeouts string) string {
	return fmt.Sprintf("server:\n"+
		"  port: 8096\n"+
		"  name: \"Test\"\n"+
		"  id: \"svr\"\n"+
		"admin:\n"+
		"  username: \"admin\"\n"+
		"  password: \"secret\"\n"+
		"playback:\n"+
		"  mode: \"proxy\"\n"+
		"timeouts:\n"+
		"  api: 30000\n"+
		"  global: 15000\n"+
		"  login: 10000\n"+
		"  healthCheck: 10000\n"+
		"  healthInterval: 60000%s\n"+
		"proxies: []\n"+
		"upstream:\n"+
		"  - name: \"A\"\n"+
		"    url: %q\n"+
		"    username: \"u1\"\n"+
		"    password: \"p1\"\n"+
		"  - name: \"B\"\n"+
		"    url: %q\n"+
		"    username: \"u2\"\n"+
		"    password: \"p2\"\n"+
		"  - name: \"C\"\n"+
		"    url: %q\n"+
		"    username: \"u3\"\n"+
		"    password: \"p3\"\n",
		extraTimeouts, url1, url2, url3)
}

func TestPhase2ViewsDefaultGraceRetainsMultiUpstreamSlowerThan150ms(t *testing.T) {
	a := phase2DelayedViewsSource(t, 20*time.Millisecond, "lib-a", "Movies A")
	b := phase2DelayedViewsSource(t, 250*time.Millisecond, "lib-b", "Movies B")
	c := phase2DelayedViewsSource(t, 400*time.Millisecond, "lib-c", "Movies C")
	withTempAppConfig(t, tripleUpstreamConfig(a.URL, b.URL, c.URL, ""), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		start := time.Now()
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		elapsed := time.Since(start)
		if rr.Code != http.StatusOK {
			t.Fatalf("Views status=%d body=%s", rr.Code, rr.Body.String())
		}
		if elapsed >= 1500*time.Millisecond {
			t.Fatalf("Views waited %v instead of returning once all 3 upstreams responded", elapsed)
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 3 {
			t.Fatalf("expected 3 libraries across all upstreams, got %d: %+v", len(items), items)
		}
		seenIDs := make(map[string]struct{}, 3)
		for i, suffix := range []string{" (A)", " (B)", " (C)"} {
			name, _ := items[i]["Name"].(string)
			if !strings.HasSuffix(name, suffix) {
				t.Fatalf("library %d name %q does not preserve configured order", i, name)
			}
			id, _ := items[i]["Id"].(string)
			if id == "" {
				t.Fatalf("library %d missing virtual ID", i)
			}
			if _, dup := seenIDs[id]; dup {
				t.Fatalf("duplicate virtual ID %q at index %d", id, i)
			}
			seenIDs[id] = struct{}{}
		}
	})
}

func TestPhase2ViewsGracePeriodStillCapsHungUpstreamWhileKeepingNormalSources(t *testing.T) {
	a := phase2DelayedViewsSource(t, 10*time.Millisecond, "lib-a", "Movies A")
	b := phase2DelayedViewsSource(t, 250*time.Millisecond, "lib-b", "Movies B")
	c := phase1SenPlayerViewsSource(t, "blocked")
	cfg := tripleUpstreamConfig(a.URL, b.URL, c.URL, "\n  metadataGracePeriod: 500")
	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		start := time.Now()
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		elapsed := time.Since(start)
		if rr.Code != http.StatusOK {
			t.Fatalf("Views status=%d body=%s", rr.Code, rr.Body.String())
		}
		if elapsed >= 2*time.Second {
			t.Fatalf("Views waited %v for hung upstream C", elapsed)
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 2 {
			t.Fatalf("expected 2 libraries (A and B), got %d: %+v", len(items), items)
		}
		for i, suffix := range []string{" (A)", " (B)"} {
			name, _ := items[i]["Name"].(string)
			if !strings.HasSuffix(name, suffix) {
				t.Fatalf("library %d name %q does not match expected %q", i, name, suffix)
			}
		}
	})
}

func TestPhase2ViewsZeroGracePeriodWaitsForSlowSourceUpToGlobalTimeout(t *testing.T) {
	fast := phase1SenPlayerViewsSource(t, "ok")
	slow := phase2DelayedViewsSource(t, 300*time.Millisecond, "slow-lib", "Slow Movies")
	config := strings.Replace(dualUpstreamConfig(fast.URL, slow.URL), "global: 15000", "global: 15000\n  metadataGracePeriod: 100", 1)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		updateRR := doJSONRequest(t, handler, http.MethodPut, "/admin/api/settings",
			map[string]any{"timeouts": map[string]any{"metadataGracePeriod": 0}}, token)
		if updateRR.Code != http.StatusOK {
			t.Fatalf("settings update status=%d body=%s", updateRR.Code, updateRR.Body.String())
		}
		if got := app.ConfigStore.Snapshot().Timeouts.MetadataGracePeriod; got != 0 {
			t.Fatalf("expected runtime metadataGracePeriod=0, got %d", got)
		}
		rr := phase1SenPlayerViewsRequest(handler, token, app.Auth.ProxyUserID(), context.Background())
		if rr.Code != http.StatusOK {
			t.Fatalf("Views status=%d body=%s", rr.Code, rr.Body.String())
		}
		items := phase1SenPlayerViewsItems(t, rr)
		if len(items) != 2 {
			t.Fatalf("expected both fast and slow libraries when metadataGracePeriod=0, got %d: %+v", len(items), items)
		}
		for i, suffix := range []string{" (A)", " (B)"} {
			name, _ := items[i]["Name"].(string)
			if !strings.HasSuffix(name, suffix) {
				t.Fatalf("library %d name %q does not preserve order", i, name)
			}
		}
	})
}
