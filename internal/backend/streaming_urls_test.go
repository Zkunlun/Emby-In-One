package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// configWithStreamingURLs builds a single-upstream config whose stream bases are
// the given ordered list (flow-sequence syntax, as renderConfigYAML writes it).
func configWithStreamingURLs(url string, streamingURLs ...string) string {
	base := singleUpstreamConfig(url)
	if len(streamingURLs) == 0 {
		return base
	}
	if len(streamingURLs) == 1 {
		return base + fmt.Sprintf("    streamingUrl: %q\n", streamingURLs[0])
	}
	quoted := make([]string, 0, len(streamingURLs))
	for _, s := range streamingURLs {
		quoted = append(quoted, "'"+s+"'")
	}
	return base + "    streamingUrls: [" + strings.Join(quoted, ", ") + "]\n"
}

// streamingUpstream answers login and counts stream requests per listening
// address, so a test can tell which stream base served a segment.
func streamingUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath:
			// Background liveness traffic is not a client media request and must not
			// affect the segment-hit assertions used by playback failover tests.
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/Videos/"):
			if strings.HasSuffix(r.URL.Path, ".m3u8") {
				w.Header().Set("Content-Type", "application/x-mpegURL")
				_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
				return
			}
			hits.Add(1)
			_, _ = w.Write([]byte("segment-body"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func streamClientForBases(t *testing.T, apiURL string, streamURLs ...string) *UpstreamClient {
	t.Helper()
	cfg, err := parseConfigYAML(configWithStreamingURLs(apiURL, streamURLs...))
	if err != nil {
		t.Fatalf("parse stream config: %v", err)
	}
	normalizeUpstream(&cfg.Upstream[0], 0, cfg)
	client := newUpstreamClient(*cfg, cfg.Upstream[0], 0, nil)
	client.setOnline("stream-token", "stream-user")
	return client
}

func redirectConfigWithStreamingURLs(url string, streamingURLs ...string) string {
	return configWithStreamingURLs(url, streamingURLs...) + "    playbackMode: \"redirect\"\n"
}

func redirectProbeUpstream(t *testing.T, probeStatus int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var probeHits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath:
			probeHits.Add(1)
			w.WriteHeader(probeStatus)
		case strings.HasPrefix(r.URL.Path, "/Videos/"):
			_, _ = w.Write([]byte("segment-body"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &probeHits
}

func doRedirectStreamRequest(t *testing.T, app *App, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	client := app.Upstream.GetClient(0)
	token := loginToken(t, handler, "secret")
	virtual := app.IDStore.GetOrCreateVirtualID("episode-redirect", client.ID)
	req := httptest.NewRequest(http.MethodGet, "/Videos/"+virtual+"/segment.ts?api_key="+token, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// A dead primary stream base must not stop playback when a fallback exists: the
// stream request is retried against the next base in order.
func TestStreamFailoverToSecondBase(t *testing.T) {
	upstream, hits := streamingUpstream(t)
	// 127.0.0.1:1 refuses connections immediately.
	config := configWithStreamingURLs(upstream.URL, "http://127.0.0.1:1", upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		client := app.Upstream.GetClient(0)
		if len(client.StreamBaseURLs) != 2 {
			t.Fatalf("stream bases = %v, want 2 entries", client.StreamBaseURLs)
		}
		token := loginToken(t, handler, "secret")
		virtual := app.IDStore.GetOrCreateVirtualID("episode-1", client.ID)

		req := httptest.NewRequest(http.MethodGet, "/Videos/"+virtual+"/segment1.ts?api_key="+token, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
		}
		if rr.Body.String() != "segment-body" {
			t.Fatalf("unexpected segment body: %q", rr.Body.String())
		}
		if hits.Load() != 1 {
			t.Fatalf("upstream segment hits = %d, want 1", hits.Load())
		}
		// The failed primary must now be marked dead so the next request goes
		// straight to the live fallback.
		candidates := client.streamBaseCandidates()
		if len(candidates) != 1 || candidates[0] != upstream.URL {
			t.Fatalf("candidates after failover = %v, want only the live base", candidates)
		}
	})
}

// A connect failure on the only stream base is reported to the client as 502
// and does not loop or panic.
func TestStreamFailoverSingleBaseStillErrors(t *testing.T) {
	upstream, _ := streamingUpstream(t)
	config := configWithStreamingURLs(upstream.URL, "http://127.0.0.1:1")

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		virtual := app.IDStore.GetOrCreateVirtualID("episode-1", app.Upstream.GetClient(0).ID)

		req := httptest.NewRequest(http.MethodGet, "/Videos/"+virtual+"/segment1.ts?api_key="+token, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502, body=%s", rr.Code, rr.Body.String())
		}
	})
}

// A 404 from the upstream is an HTTP answer, not a dead line: no failover is
// attempted and no base is marked dead.
func TestStreamFailoverDoesNotTriggerOnHTTPStatus(t *testing.T) {
	var statusHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath:
			// Keep background liveness traffic out of the playback-request counter.
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/Videos/"):
			statusHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	fallback, fallbackHits := streamingUpstream(t)
	config := configWithStreamingURLs(upstream.URL, upstream.URL, fallback.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		client := app.Upstream.GetClient(0)
		token := loginToken(t, handler, "secret")
		virtual := app.IDStore.GetOrCreateVirtualID("episode-1", client.ID)

		req := httptest.NewRequest(http.MethodGet, "/Videos/"+virtual+"/segment1.ts?api_key="+token, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (the upstream's own answer)", rr.Code)
		}
		if statusHits.Load() != 1 {
			t.Fatalf("primary hits = %d, want 1 (no retry after an HTTP response)", statusHits.Load())
		}
		if fallbackHits.Load() != 0 {
			t.Fatalf("fallback hits = %d, want 0 (failover must not fire on an HTTP status)", fallbackHits.Load())
		}
		if candidates := client.streamBaseCandidates(); len(candidates) != 2 {
			t.Fatalf("candidates = %v, want both bases still live", candidates)
		}
	})
}

// Proxy playback must treat gateway/service-unavailable responses the same way
// as the background liveness probe: mark that base dead and continue to the next
// stream base within the same client request.
func TestProxyStreamFailoverOnGatewayStatuses(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var primaryHits atomic.Int64
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits.Add(1)
				w.WriteHeader(status)
			}))
			defer primary.Close()

			fallback, fallbackHits := streamingUpstream(t)
			client := streamClientForBases(t, primary.URL, primary.URL, fallback.URL)

			resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
			if err != nil {
				t.Fatalf("Stream returned error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("final status = %d, want 200 from fallback", resp.StatusCode)
			}
			if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
				t.Fatalf("stream hits primary=%d fallback=%d, want 1/1", primaryHits.Load(), fallbackHits.Load())
			}
			client.mu.RLock()
			primaryHealth := client.streamHealth[primary.URL].state
			fallbackHealth := client.streamHealth[fallback.URL].state
			client.mu.RUnlock()
			if primaryHealth != streamBaseDead || fallbackHealth != streamBaseAlive {
				t.Fatalf("health primary=%v fallback=%v, want dead/alive", primaryHealth, fallbackHealth)
			}
		})
	}
}

// If every attempted proxy stream base returns a classified unavailable status,
// the upstream response is consumed internally and Stream must return an error so
// the EIO handler emits its own 502 rather than forwarding a known-broken 503/504.
func TestProxyStreamAllGatewayStatusesReturnError(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer second.Close()

	client := streamClientForBases(t, first.URL, first.URL, second.URL)
	resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("Stream returned upstream response %d after all bases were unavailable", resp.StatusCode)
	}
	if err == nil {
		t.Fatal("Stream returned nil error after all bases were unavailable")
	}
	client.mu.RLock()
	firstState := client.streamHealth[first.URL].state
	secondState := client.streamHealth[second.URL].state
	client.mu.RUnlock()
	if firstState != streamBaseDead || secondState != streamBaseDead {
		t.Fatalf("health first=%v second=%v, want dead/dead", firstState, secondState)
	}
}

// HTTP responses that are not classified as stream-unavailable must remain the
// upstream's own response and must not trigger fallback. Phase 2C deliberately
// leaves 500 reachable, while 404 is the existing route-reachable case.
func TestProxyStreamDoesNotFailoverOnReachableHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var primaryHits atomic.Int64
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits.Add(1)
				w.WriteHeader(status)
			}))
			defer primary.Close()

			fallback, fallbackHits := streamingUpstream(t)
			client := streamClientForBases(t, primary.URL, primary.URL, fallback.URL)
			resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
			if err != nil {
				t.Fatalf("Stream returned error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != status {
				t.Fatalf("final status = %d, want primary status %d", resp.StatusCode, status)
			}
			if primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
				t.Fatalf("stream hits primary=%d fallback=%d, want 1/0", primaryHits.Load(), fallbackHits.Load())
			}
		})
	}
}

// Once every proxy stream base is known dead and still inside cooldown, a player
// retry must not immediately hammer every failed line again. No outbound media
// request is eligible until at least one base reaches revalidation time.
func TestProxyAllDeadCoolingDownDoesNotRetryStreamBases(t *testing.T) {
	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	client := streamClientForBases(t, first.URL, first.URL, second.URL)
	client.markStreamBaseFailed(first.URL)
	client.markStreamBaseFailed(second.URL)

	resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("Stream returned response status %d while every base was cooling down", resp.StatusCode)
	}
	if err == nil {
		t.Fatal("Stream succeeded while every base was dead and cooling down")
	}
	if firstHits.Load() != 0 || secondHits.Load() != 0 {
		t.Fatalf("cooldown retry hit dead bases first=%d second=%d, want 0/0", firstHits.Load(), secondHits.Load())
	}
}

// If all bases are dead but only one has reached recovery eligibility, proxy
// playback may use that line as a controlled recovery attempt; younger dead
// lines must stay untouched.
func TestProxyAllDeadUsesOnlyCooldownEligibleRecoveryBase(t *testing.T) {
	var recentHits, eligibleHits atomic.Int64
	recent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recentHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer recent.Close()
	eligible := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eligibleHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer eligible.Close()

	client := streamClientForBases(t, recent.URL, recent.URL, eligible.URL)
	client.markStreamBaseFailed(recent.URL)
	client.markStreamBaseFailed(eligible.URL)
	client.mu.Lock()
	health := client.streamHealth[eligible.URL]
	health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
	client.streamHealth[eligible.URL] = health
	client.mu.Unlock()

	resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if err != nil {
		t.Fatalf("eligible recovery Stream returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery status = %d, want 200", resp.StatusCode)
	}
	if recentHits.Load() != 0 || eligibleHits.Load() != 1 {
		t.Fatalf("recovery hits recent=%d eligible=%d, want 0/1", recentHits.Load(), eligibleHits.Load())
	}
	client.mu.RLock()
	recentState := client.streamHealth[recent.URL].state
	eligibleState := client.streamHealth[eligible.URL].state
	client.mu.RUnlock()
	if recentState != streamBaseDead || eligibleState != streamBaseAlive {
		t.Fatalf("health recent=%v eligible=%v, want dead/alive", recentState, eligibleState)
	}
}

// A dead primary does not regain proxy priority merely because its cooldown has
// elapsed while another usable base still exists. Background health probing must
// first provide recovery evidence for the dead line.
func TestProxyAgedDeadBaseStaysExcludedWhileLiveBaseExists(t *testing.T) {
	var deadHits, liveHits atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer dead.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		liveHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	client := streamClientForBases(t, dead.URL, dead.URL, live.URL)
	client.markStreamBaseFailed(dead.URL)
	client.markStreamBaseAlive(live.URL)
	client.mu.Lock()
	health := client.streamHealth[dead.URL]
	health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
	client.streamHealth[dead.URL] = health
	client.mu.Unlock()

	resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer resp.Body.Close()
	if deadHits.Load() != 0 || liveHits.Load() != 1 {
		t.Fatalf("aged-dead/live hits = %d/%d, want 0/1", deadHits.Load(), liveHits.Load())
	}
}

// When every dead line is recovery-eligible, proxy playback tries them in the
// configured order and stops at the first recovered line. A failed recovery is
// re-marked dead, while later lines remain untouched after success.
func TestProxyAllDeadRecoveryStopsAtFirstSuccessfulEligibleBase(t *testing.T) {
	var firstHits, secondHits, thirdHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer third.Close()

	client := streamClientForBases(t, first.URL, first.URL, second.URL, third.URL)
	for _, base := range client.StreamBaseURLs {
		client.markStreamBaseFailed(base)
	}
	client.mu.Lock()
	for _, base := range client.StreamBaseURLs {
		health := client.streamHealth[base]
		health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[base] = health
	}
	client.mu.Unlock()

	resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if err != nil {
		t.Fatalf("recovery Stream returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery status = %d, want 200", resp.StatusCode)
	}
	if firstHits.Load() != 1 || secondHits.Load() != 1 || thirdHits.Load() != 0 {
		t.Fatalf("recovery hits first=%d second=%d third=%d, want 1/1/0", firstHits.Load(), secondHits.Load(), thirdHits.Load())
	}
	client.mu.RLock()
	firstState := client.streamHealth[first.URL].state
	secondState := client.streamHealth[second.URL].state
	thirdState := client.streamHealth[third.URL].state
	client.mu.RUnlock()
	if firstState != streamBaseDead || secondState != streamBaseAlive || thirdState != streamBaseDead {
		t.Fatalf("recovery health first=%v second=%v third=%v, want dead/alive/dead", firstState, secondState, thirdState)
	}
}

// Background probing intentionally skips a single configured stream base, while
// the shared execution layer must still be able to probe one explicit base for a
// future request-scoped recovery path.
func TestProbeSelectedStreamBasesSupportsSingleExplicitBase(t *testing.T) {
	only, probeHits := redirectProbeUpstream(t, http.StatusNotFound)
	client := streamClientForBases(t, only.URL, only.URL)
	client.markStreamBaseFailed(only.URL)
	client.mu.Lock()
	health := client.streamHealth[only.URL]
	health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
	client.streamHealth[only.URL] = health
	client.mu.Unlock()

	client.probeStreamBases(context.Background())
	if probeHits.Load() != 0 {
		t.Fatalf("background single-base probes = %d, want 0", probeHits.Load())
	}

	client.probeSelectedStreamBases(context.Background(), []string{only.URL}, 2*time.Second)
	if probeHits.Load() != 1 {
		t.Fatalf("explicit single-base probes = %d, want 1", probeHits.Load())
	}
	client.mu.RLock()
	state := client.streamHealth[only.URL].state
	client.mu.RUnlock()
	if state != streamBaseAlive {
		t.Fatalf("explicit single-base state = %v, want alive", state)
	}
}

// The shared executor must honor the caller's exact subset rather than falling
// back to the background candidate policy or probing every configured line.
func TestProbeSelectedStreamBasesUsesExactExplicitSubset(t *testing.T) {
	first, firstHits := redirectProbeUpstream(t, http.StatusNotFound)
	second, secondHits := redirectProbeUpstream(t, http.StatusNotFound)
	client := streamClientForBases(t, first.URL, first.URL, second.URL)
	client.markStreamBaseFailed(first.URL)
	client.markStreamBaseFailed(second.URL)

	client.probeSelectedStreamBases(context.Background(), []string{second.URL}, 2*time.Second)
	if firstHits.Load() != 0 || secondHits.Load() != 1 {
		t.Fatalf("explicit subset probes first=%d second=%d, want 0/1", firstHits.Load(), secondHits.Load())
	}
	client.mu.RLock()
	firstState := client.streamHealth[first.URL].state
	secondState := client.streamHealth[second.URL].state
	client.mu.RUnlock()
	if firstState != streamBaseDead || secondState != streamBaseAlive {
		t.Fatalf("explicit subset health first=%v second=%v, want dead/alive", firstState, secondState)
	}
}

func TestRedirectRecoveryProbeTimeoutIsCapped(t *testing.T) {
	client := streamClientForBases(t, "http://api.example", "http://stream.example")
	client.timeouts.HealthCheck = 10_000
	if got := client.redirectRecoveryProbeTimeout(); got != redirectRecoveryProbeCap {
		t.Fatalf("redirect recovery timeout = %s, want cap %s", got, redirectRecoveryProbeCap)
	}
	client.timeouts.HealthCheck = 2_500
	if got := client.redirectRecoveryProbeTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("redirect recovery timeout = %s, want configured 2.5s", got)
	}
}

func TestBuildURLRefusesKnownDeadStreamFallback(t *testing.T) {
	first := "http://first.example"
	second := "http://second.example"
	client := streamClientForBases(t, first, first, second)
	client.markStreamBaseFailed(first)
	client.markStreamBaseFailed(second)

	if _, err := client.BuildURL("/Videos/item/segment.ts", nil, true, nil); !errors.Is(err, errNoUsableStreamBase) {
		t.Fatalf("BuildURL all-dead error = %v, want errNoUsableStreamBase", err)
	}
}

// Redirect must not hand the client a line that EIO already knows is dead. If
// every line is still cooling down, no recovery probe is eligible and playback
// fails locally instead of falling back to the first configured URL.
func TestRedirectAllDeadCoolingDownReturnsBadGateway(t *testing.T) {
	first, firstProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	second, secondProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	config := redirectConfigWithStreamingURLs(first.URL, first.URL, second.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		firstProbeHits.Store(0)
		secondProbeHits.Store(0)
		client.markStreamBaseFailed(first.URL)
		client.markStreamBaseFailed(second.URL)

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 while every redirect line cools down; Location=%q", rr.Code, rr.Header().Get("Location"))
		}
		if got := rr.Header().Get("Location"); got != "" {
			t.Fatalf("all-dead cooldown unexpectedly redirected to %q", got)
		}
		if firstProbeHits.Load() != 0 || secondProbeHits.Load() != 0 {
			t.Fatalf("cooldown redirect triggered probes first=%d second=%d, want 0/0", firstProbeHits.Load(), secondProbeHits.Load())
		}
	})
}

// When all redirect lines are dead, only cooldown-expired lines may be probed.
// A successful request-scoped probe revives that line and the same request then
// redirects to it; younger dead lines stay untouched.
func TestRedirectAllDeadRecoversOnlyEligibleBaseBeforeRedirect(t *testing.T) {
	recent, recentProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	eligible, eligibleProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	config := redirectConfigWithStreamingURLs(recent.URL, recent.URL, eligible.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		recentProbeHits.Store(0)
		eligibleProbeHits.Store(0)
		client.markStreamBaseFailed(recent.URL)
		client.markStreamBaseFailed(eligible.URL)
		client.mu.Lock()
		health := client.streamHealth[eligible.URL]
		health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[eligible.URL] = health
		client.mu.Unlock()

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 after eligible redirect line recovers, body=%s", rr.Code, rr.Body.String())
		}
		if location := rr.Header().Get("Location"); !strings.HasPrefix(location, eligible.URL+"/Videos/") {
			t.Fatalf("Location = %q, want recovered base %s", location, eligible.URL)
		}
		if recentProbeHits.Load() != 0 || eligibleProbeHits.Load() != 1 {
			t.Fatalf("recovery probes recent=%d eligible=%d, want 0/1", recentProbeHits.Load(), eligibleProbeHits.Load())
		}
		client.mu.RLock()
		recentState := client.streamHealth[recent.URL].state
		eligibleState := client.streamHealth[eligible.URL].state
		client.mu.RUnlock()
		if recentState != streamBaseDead || eligibleState != streamBaseAlive {
			t.Fatalf("health recent=%v eligible=%v, want dead/alive", recentState, eligibleState)
		}
	})
}

func TestRedirectAllDeadEligibleProbeFailureReturnsBadGateway(t *testing.T) {
	recent, recentProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	eligible, eligibleProbeHits := redirectProbeUpstream(t, http.StatusServiceUnavailable)
	config := redirectConfigWithStreamingURLs(recent.URL, recent.URL, eligible.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		recentProbeHits.Store(0)
		eligibleProbeHits.Store(0)
		client.markStreamBaseFailed(recent.URL)
		client.markStreamBaseFailed(eligible.URL)
		client.mu.Lock()
		health := client.streamHealth[eligible.URL]
		health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[eligible.URL] = health
		client.mu.Unlock()

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 after recovery probe fails; Location=%q", rr.Code, rr.Header().Get("Location"))
		}
		if recentProbeHits.Load() != 0 || eligibleProbeHits.Load() != 1 {
			t.Fatalf("failed recovery probes recent=%d eligible=%d, want 0/1", recentProbeHits.Load(), eligibleProbeHits.Load())
		}
		client.mu.RLock()
		eligibleHealth := client.streamHealth[eligible.URL]
		client.mu.RUnlock()
		if eligibleHealth.state != streamBaseDead {
			t.Fatalf("eligible state = %v, want dead after failed redirect recovery probe", eligibleHealth.state)
		}
	})
}

// Unknown is not failure evidence. Redirect startup must keep its zero-wait fast
// path and hand the client the first unknown line without a synchronous probe.
func TestRedirectUnknownBaseStillRedirectsWithoutSynchronousProbe(t *testing.T) {
	first, firstProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	second, secondProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	config := redirectConfigWithStreamingURLs(first.URL, first.URL, second.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		firstProbeHits.Store(0)
		secondProbeHits.Store(0)
		client.mu.Lock()
		client.streamHealth[first.URL] = streamBaseHealth{state: streamBaseUnknown}
		client.streamHealth[second.URL] = streamBaseHealth{state: streamBaseUnknown}
		client.mu.Unlock()

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 for unknown redirect line", rr.Code)
		}
		if location := rr.Header().Get("Location"); !strings.HasPrefix(location, first.URL+"/Videos/") {
			t.Fatalf("Location = %q, want first unknown base %s", location, first.URL)
		}
		if firstProbeHits.Load() != 0 || secondProbeHits.Load() != 0 {
			t.Fatalf("unknown redirect triggered synchronous probes first=%d second=%d, want 0/0", firstProbeHits.Load(), secondProbeHits.Load())
		}
	})
}

// A cooldown-expired dead primary must not trigger request-scoped recovery while
// another usable line exists. Background probing is responsible for restoring it.
func TestRedirectAgedDeadBaseStaysExcludedWhileUsableBaseExists(t *testing.T) {
	dead, deadProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	live, liveProbeHits := redirectProbeUpstream(t, http.StatusNotFound)
	config := redirectConfigWithStreamingURLs(dead.URL, dead.URL, live.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		deadProbeHits.Store(0)
		liveProbeHits.Store(0)
		client.markStreamBaseFailed(dead.URL)
		client.markStreamBaseAlive(live.URL)
		client.mu.Lock()
		health := client.streamHealth[dead.URL]
		health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[dead.URL] = health
		client.mu.Unlock()

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 to existing usable redirect base", rr.Code)
		}
		if location := rr.Header().Get("Location"); !strings.HasPrefix(location, live.URL+"/Videos/") {
			t.Fatalf("Location = %q, want live base %s", location, live.URL)
		}
		if deadProbeHits.Load() != 0 || liveProbeHits.Load() != 0 {
			t.Fatalf("usable-base redirect triggered recovery probes dead=%d live=%d, want 0/0", deadProbeHits.Load(), liveProbeHits.Load())
		}
	})
}

// Single-line redirect setups need the same recovery path. The background probe
// intentionally skips one-base configurations, so request-scoped recovery must be
// able to validate that one dead line after cooldown.
func TestRedirectSingleDeadBaseCanRecoverAfterCooldown(t *testing.T) {
	only, probeHits := redirectProbeUpstream(t, http.StatusNotFound)
	config := redirectConfigWithStreamingURLs(only.URL, only.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		probeHits.Store(0)
		client.markStreamBaseFailed(only.URL)
		client.mu.Lock()
		health := client.streamHealth[only.URL]
		health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[only.URL] = health
		client.mu.Unlock()

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 after single redirect base recovers", rr.Code)
		}
		if location := rr.Header().Get("Location"); !strings.HasPrefix(location, only.URL+"/Videos/") {
			t.Fatalf("Location = %q, want single recovered base %s", location, only.URL)
		}
		if probeHits.Load() != 1 {
			t.Fatalf("single-base recovery probes = %d, want 1", probeHits.Load())
		}
		client.mu.RLock()
		state := client.streamHealth[only.URL].state
		client.mu.RUnlock()
		if state != streamBaseAlive {
			t.Fatalf("single recovered base state = %v, want alive", state)
		}
	})
}

// Eligible all-dead redirect lines are probed together so request latency is
// bounded by one probe timeout rather than N sequential timeouts. Selection after
// recovery still follows configured order among the lines proven alive.
func TestRedirectAllDeadRecoveryProbesEligibleBasesConcurrently(t *testing.T) {
	var enabled atomic.Bool
	var firstProbeHits, secondProbeHits atomic.Int64
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	var firstSawSecond atomic.Bool

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.URL.Path == streamLivenessProbePath:
			if !enabled.Load() {
				http.NotFound(w, r)
				return
			}
			firstProbeHits.Add(1)
			select {
			case firstStarted <- struct{}{}:
			default:
			}
			select {
			case <-secondStarted:
				firstSawSecond.Store(true)
			case <-time.After(500 * time.Millisecond):
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != streamLivenessProbePath {
			http.NotFound(w, r)
			return
		}
		if !enabled.Load() {
			http.NotFound(w, r)
			return
		}
		secondProbeHits.Add(1)
		select {
		case secondStarted <- struct{}{}:
		default:
		}
		select {
		case <-firstStarted:
		case <-time.After(500 * time.Millisecond):
		}
		http.NotFound(w, r)
	}))
	defer second.Close()

	config := redirectConfigWithStreamingURLs(first.URL, first.URL, second.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		app.Upstream.stopHealthChecks()
		client := app.Upstream.GetClient(0)
		client.markStreamBaseFailed(first.URL)
		client.markStreamBaseFailed(second.URL)
		client.mu.Lock()
		firstHealth := client.streamHealth[first.URL]
		firstHealth.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[first.URL] = firstHealth
		secondHealth := client.streamHealth[second.URL]
		secondHealth.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[second.URL] = secondHealth
		client.mu.Unlock()
		enabled.Store(true)

		rr := doRedirectStreamRequest(t, app, handler)
		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 after concurrent redirect recovery", rr.Code)
		}
		if location := rr.Header().Get("Location"); !strings.HasPrefix(location, second.URL+"/Videos/") {
			t.Fatalf("Location = %q, want second recovered base %s", location, second.URL)
		}
		if firstProbeHits.Load() != 1 || secondProbeHits.Load() != 1 {
			t.Fatalf("concurrent recovery probes first=%d second=%d, want 1/1", firstProbeHits.Load(), secondProbeHits.Load())
		}
		if !firstSawSecond.Load() {
			t.Fatal("first redirect recovery probe completed without observing the second probe start; want concurrent probing")
		}
	})
}

// Liveness keeps explicit unknown/alive/dead state. A 403 from a split-tunnel
// route proves that line alive, while a connect failure marks the other line
// dead. Time passing only makes a dead line eligible for another probe; it does
// not make that line a playback candidate again without successful evidence.
func TestStreamProbeLivenessSemantics(t *testing.T) {
	upstream, _ := streamingUpstream(t)
	deadPrimary := "http://127.0.0.1:1"
	// A "split tunnel" that answers 403 for everything: alive by probe rules.
	var probeHits atomic.Int64
	splitTunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeHits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer splitTunnel.Close()

	config := configWithStreamingURLs(upstream.URL, deadPrimary, splitTunnel.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		client := app.Upstream.GetClient(0)
		client.probeStreamBases(context.Background())

		candidates := client.streamBaseCandidates()
		if len(candidates) != 1 || candidates[0] != splitTunnel.URL {
			t.Fatalf("candidates = %v, want only the split-tunnel base (dead primary pruned, 403 line alive)", candidates)
		}
		if probeHits.Load() == 0 {
			t.Fatalf("the split-tunnel line was never probed")
		}
		if probeCandidates := client.streamBaseProbeCandidates(time.Now()); len(probeCandidates) != 1 || probeCandidates[0] != splitTunnel.URL {
			t.Fatalf("probe candidates during dead cooldown = %v, want only the alive split-tunnel base", probeCandidates)
		}

		// Redirect playback candidates contain only lines with actual usability
		// evidence. Once every line is dead, no dead fallback may leak back in.
		client.markStreamBaseFailed(splitTunnel.URL)
		if all := client.streamBaseCandidates(); len(all) != 0 {
			t.Fatalf("all-dead playback candidates = %v, want none", all)
		}

		// Age only the dead primary. It becomes eligible for revalidation, but
		// remains excluded from playback until a successful probe/request marks it alive.
		client.mu.Lock()
		health := client.streamHealth[deadPrimary]
		health.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
		client.streamHealth[deadPrimary] = health
		client.mu.Unlock()
		client.markStreamBaseAlive(splitTunnel.URL)

		if candidates := client.streamBaseCandidates(); len(candidates) != 1 || candidates[0] != splitTunnel.URL {
			t.Fatalf("aged dead base became a playback candidate without recovery evidence: %v", candidates)
		}
		probeCandidates := client.streamBaseProbeCandidates(time.Now())
		if len(probeCandidates) != 2 || probeCandidates[0] != deadPrimary || probeCandidates[1] != splitTunnel.URL {
			t.Fatalf("post-cooldown probe candidates = %v, want dead primary revalidation plus alive fallback", probeCandidates)
		}

		client.markStreamBaseAlive(deadPrimary)
		if revived := client.streamBaseCandidates(); len(revived) != 2 {
			t.Fatalf("successfully revived candidates = %v, want both bases", revived)
		}
	})
}

// An online upstream must still probe its API base when that same URL is also
// configured as the primary stream line. The regular health cycle skips login
// checks for online upstreams, so excluding this base would leave the primary
// redirect line permanently unobserved in the background.
func TestHealthCycleProbesAPIBaseWhenAlsoPrimaryStreamBase(t *testing.T) {
	var apiProbeHits atomic.Int64
	apiBase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath:
			apiProbeHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiBase.Close()

	var fallbackProbeHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath {
			fallbackProbeHits.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer fallback.Close()

	config := configWithStreamingURLs(apiBase.URL, apiBase.URL, fallback.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		client := app.Upstream.GetClient(0)
		if client == nil || !client.IsOnline() {
			t.Fatalf("upstream must be online before the health cycle")
		}

		waitForCondition(t, 2*time.Second, func() bool {
			return apiProbeHits.Load() >= 1 && fallbackProbeHits.Load() >= 1
		}, "initial stream probe")
		apiBefore := apiProbeHits.Load()
		fallbackBefore := fallbackProbeHits.Load()

		app.Upstream.runHealthCheckCycle(context.Background())

		if got := apiProbeHits.Load(); got != apiBefore+1 {
			t.Errorf("API/primary stream base probe hits = %d, want %d", got, apiBefore+1)
		}
		if got := fallbackProbeHits.Load(); got != fallbackBefore+1 {
			t.Errorf("fallback stream base probe hits = %d, want %d", got, fallbackBefore+1)
		}
	})
}

// Initial login must kick a stream-line probe immediately rather than wait for
// the first 60-second health ticker. This closes the startup window where redirect
// mode would otherwise select an unobserved primary line.
func TestInitialLoginTriggersStreamProbeBeforeFirstInterval(t *testing.T) {
	var primaryProbeHits atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath:
			primaryProbeHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer primary.Close()

	var fallbackProbeHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath {
			fallbackProbeHits.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer fallback.Close()

	config := configWithStreamingURLs(primary.URL, primary.URL, fallback.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		waitForCondition(t, 2*time.Second, func() bool {
			return primaryProbeHits.Load() >= 1 && fallbackProbeHits.Load() >= 1
		}, "initial stream probe before the 60-second health interval")
	})
}

// Reloading the pool with an already-online upstream must also kick a fresh
// stream-line probe immediately. A settings/config save must not leave the new
// client set waiting for the next periodic ticker.
func TestReloadTriggersStreamProbeBeforeNextInterval(t *testing.T) {
	var primaryProbeHits atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "stream-token", "User": map[string]any{"Id": "stream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath:
			primaryProbeHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fallback.Close()

	config := configWithStreamingURLs(primary.URL, primary.URL, fallback.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		waitForCondition(t, 2*time.Second, func() bool { return primaryProbeHits.Load() >= 1 }, "initial stream probe")
		before := primaryProbeHits.Load()
		app.Upstream.Reload(app.ConfigStore.Snapshot())
		waitForCondition(t, 2*time.Second, func() bool { return primaryProbeHits.Load() > before }, "stream probe immediately after reload")
	})
}

// Reload rebuilds UpstreamClient objects, but unchanged stream bases must keep
// their explicit health state. Otherwise a known-dead line temporarily becomes
// unknown/playable until the asynchronous post-reload probe finishes.
func TestReloadPreservesStreamHealthForUnchangedBases(t *testing.T) {
	first := "http://stream-a.example"
	second := "http://stream-b.example"
	cfg, err := parseConfigYAML(configWithStreamingURLs("http://api.example", first, second))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pool := NewUpstreamPool(*cfg, nil)
	defer pool.stopHealthChecks()

	old := pool.GetClient(0)
	old.markStreamBaseFailed(first)
	old.markStreamBaseAlive(second)
	old.mu.RLock()
	oldFirst := old.streamHealth[first]
	old.mu.RUnlock()

	pool.Reload(*cfg)
	current := pool.GetClient(0)
	current.mu.RLock()
	firstHealth := current.streamHealth[first]
	secondHealth := current.streamHealth[second]
	current.mu.RUnlock()
	if firstHealth.state != streamBaseDead || secondHealth.state != streamBaseAlive {
		t.Fatalf("reloaded health first=%v second=%v, want dead/alive", firstHealth.state, secondHealth.state)
	}
	if firstHealth.lastFailure.IsZero() || !firstHealth.lastFailure.Equal(oldFirst.lastFailure) {
		t.Fatalf("reloaded dead lastFailure=%v, want inherited %v", firstHealth.lastFailure, oldFirst.lastFailure)
	}
}

// Health inheritance is by stream-base identity, not by list position. Removed
// lines disappear, unchanged lines retain their state, and newly configured lines
// start unknown until real traffic or a probe observes them.
func TestReloadPreservesOnlyIntersectingStreamHealth(t *testing.T) {
	first := "http://stream-a.example"
	removed := "http://stream-b.example"
	added := "http://stream-c.example"
	cfg, err := parseConfigYAML(configWithStreamingURLs("http://api.example", first, removed))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pool := NewUpstreamPool(*cfg, nil)
	defer pool.stopHealthChecks()

	old := pool.GetClient(0)
	old.markStreamBaseFailed(first)
	old.markStreamBaseAlive(removed)

	next := *cfg
	next.Upstream = append([]UpstreamConfig(nil), cfg.Upstream...)
	next.Upstream[0].StreamingURL = ""
	next.Upstream[0].StreamingURLs = []string{first, added}
	normalizeStreamingURLs(&next.Upstream[0])
	pool.Reload(next)

	current := pool.GetClient(0)
	current.mu.RLock()
	firstHealth := current.streamHealth[first]
	addedHealth := current.streamHealth[added]
	_, removedStillPresent := current.streamHealth[removed]
	current.mu.RUnlock()
	if firstHealth.state != streamBaseDead {
		t.Fatalf("unchanged base state = %v, want dead", firstHealth.state)
	}
	if addedHealth.state != streamBaseUnknown {
		t.Fatalf("new base state = %v, want unknown", addedHealth.state)
	}
	if removedStillPresent {
		t.Fatalf("removed base %s still has inherited health state", removed)
	}
}

// An older probe result must not overwrite evidence from a media request that
// started later. Completion order is irrelevant: the newer observation owns the
// final state even if the old probe returns last.
func TestOlderProbeFailureCannotOverrideNewerStreamSuccess(t *testing.T) {
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == streamLivenessProbePath {
			select {
			case probeStarted <- struct{}{}:
			default:
			}
			<-releaseProbe
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/Videos/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := streamClientForBases(t, server.URL, server.URL)
	probeDone := make(chan struct{})
	go func() {
		client.probeSelectedStreamBases(context.Background(), []string{server.URL}, 2*time.Second)
		close(probeDone)
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		close(releaseProbe)
		<-probeDone
		t.Fatal("old probe did not start")
	}

	resp, streamErr := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if resp != nil {
		resp.Body.Close()
	}
	close(releaseProbe)
	select {
	case <-probeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("old probe did not finish")
	}
	if streamErr != nil {
		t.Fatalf("newer stream request failed: %v", streamErr)
	}
	client.mu.RLock()
	state := client.streamHealth[server.URL].state
	client.mu.RUnlock()
	if state != streamBaseAlive {
		t.Fatalf("final state = %v, want alive; older failing probe overwrote newer stream success", state)
	}
}

// Symmetric case: a probe that started first but succeeds late must not revive a
// line after a newer real media request has already proven that line unavailable.
func TestOlderProbeSuccessCannotOverrideNewerStreamFailure(t *testing.T) {
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == streamLivenessProbePath {
			select {
			case probeStarted <- struct{}{}:
			default:
			}
			<-releaseProbe
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/Videos/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := streamClientForBases(t, server.URL, server.URL)
	probeDone := make(chan struct{})
	go func() {
		client.probeSelectedStreamBases(context.Background(), []string{server.URL}, 2*time.Second)
		close(probeDone)
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		close(releaseProbe)
		<-probeDone
		t.Fatal("old probe did not start")
	}

	resp, streamErr := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if resp != nil {
		resp.Body.Close()
	}
	close(releaseProbe)
	select {
	case <-probeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("old probe did not finish")
	}
	if streamErr == nil {
		t.Fatal("newer stream request unexpectedly succeeded on 503")
	}
	client.mu.RLock()
	state := client.streamHealth[server.URL].state
	client.mu.RUnlock()
	if state != streamBaseDead {
		t.Fatalf("final state = %v, want dead; older successful probe overwrote newer stream failure", state)
	}
}

// Observation ordering is per base. Activity on B must not invalidate an in-flight
// observation on A merely because B receives a newer generation globally.
func TestStreamObservationOrderingIsIndependentPerBase(t *testing.T) {
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == streamLivenessProbePath {
			select {
			case probeStarted <- struct{}{}:
			default:
			}
			<-releaseProbe
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/Videos/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer second.Close()

	client := streamClientForBases(t, second.URL, first.URL, second.URL)
	client.markStreamBaseFailed(first.URL)
	client.markStreamBaseAlive(second.URL)
	probeDone := make(chan struct{})
	go func() {
		client.probeSelectedStreamBases(context.Background(), []string{first.URL}, 2*time.Second)
		close(probeDone)
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		close(releaseProbe)
		<-probeDone
		t.Fatal("base A probe did not start")
	}

	resp, err := client.Stream(context.Background(), nil, nil, "/Videos/item/segment.ts", nil)
	if resp != nil {
		resp.Body.Close()
	}
	close(releaseProbe)
	<-probeDone
	if err != nil {
		t.Fatalf("base B stream failed: %v", err)
	}
	client.mu.RLock()
	firstState := client.streamHealth[first.URL].state
	secondState := client.streamHealth[second.URL].state
	client.mu.RUnlock()
	if firstState != streamBaseAlive || secondState != streamBaseAlive {
		t.Fatalf("independent-base health first=%v second=%v, want alive/alive", firstState, secondState)
	}
}

// All configured stream lines for one upstream should be probed in parallel.
// Both handlers block until the test releases them; a serial implementation can
// never enter both handlers at once and therefore fails this assertion.
func TestStreamProbesConfiguredBasesConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	newBlockingLine := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == streamLivenessProbePath {
				started <- struct{}{}
				<-release
			}
			http.NotFound(w, r)
		}))
	}
	lineA := newBlockingLine()
	defer lineA.Close()
	lineB := newBlockingLine()
	defer lineB.Close()

	cfg, err := parseConfigYAML(configWithStreamingURLs(lineA.URL, lineA.URL, lineB.URL))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	normalizeUpstream(&cfg.Upstream[0], 0, cfg)
	client := newUpstreamClient(*cfg, cfg.Upstream[0], 0, nil)

	done := make(chan struct{})
	go func() {
		client.probeStreamBases(context.Background())
		close(done)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			<-done
			t.Fatalf("only %d/2 stream probes started concurrently", i)
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent stream probes did not finish")
	}
}

// Probe status classification distinguishes a reachable media route from a
// reverse proxy that is reachable but cannot reach the media service behind it.
// Authentication/not-found answers still prove the route is present; gateway
// unavailability answers must not keep a broken line eligible for redirect.
func TestStreamProbeStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantAlive bool
	}{
		{name: "OK", status: http.StatusOK, wantAlive: true},
		{name: "Unauthorized", status: http.StatusUnauthorized, wantAlive: true},
		{name: "Forbidden", status: http.StatusForbidden, wantAlive: true},
		{name: "NotFound", status: http.StatusNotFound, wantAlive: true},
		{name: "InternalServerError", status: http.StatusInternalServerError, wantAlive: true},
		{name: "BadGateway", status: http.StatusBadGateway, wantAlive: false},
		{name: "ServiceUnavailable", status: http.StatusServiceUnavailable, wantAlive: false},
		{name: "GatewayTimeout", status: http.StatusGatewayTimeout, wantAlive: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			got := probeOneStreamBase(context.Background(), server.Client(), server.URL+streamLivenessProbePath)
			if got != tc.wantAlive {
				t.Errorf("status %d: alive = %v, want %v", tc.status, got, tc.wantAlive)
			}
		})
	}
}

// The shared stream-unavailable classifier is the contract both probes and
// proxy playback must use. Phase 3C-B wires the probe to this helper; Phase
// 3C-C will make proxy request failover consume the same classification.
func TestStreamUnavailableStatusClassification(t *testing.T) {
	cases := []struct {
		status          int
		wantUnavailable bool
	}{
		{status: http.StatusNotFound, wantUnavailable: false},
		{status: http.StatusInternalServerError, wantUnavailable: false},
		{status: http.StatusBadGateway, wantUnavailable: true},
		{status: http.StatusServiceUnavailable, wantUnavailable: true},
		{status: http.StatusGatewayTimeout, wantUnavailable: true},
	}

	for _, tc := range cases {
		if got := isStreamUnavailableStatus(tc.status); got != tc.wantUnavailable {
			t.Errorf("status %d: unavailable = %v, want %v", tc.status, got, tc.wantUnavailable)
		}
	}
}

// Redirect mode picks the first live stream base for its 302 target, skipping
// bases the liveness marks say are dead.
func TestRedirectModeUsesFirstLiveBase(t *testing.T) {
	upstream, _ := streamingUpstream(t)
	dead := "http://127.0.0.1:1"
	config := configWithStreamingURLs(upstream.URL, dead, upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		client := app.Upstream.GetClient(0)
		client.Config.PlaybackMode = "redirect"
		client.markStreamBaseFailed(dead)

		redirectURL, err := client.BuildURL("/Videos/orig/segment.ts", nil, true, nil)
		if err != nil {
			t.Fatalf("BuildURL: %v", err)
		}
		if !strings.HasPrefix(redirectURL, upstream.URL+"/Videos/orig/segment.ts") {
			t.Fatalf("redirect URL = %q, want the live base %s", redirectURL, upstream.URL)
		}
	})
}

// The manifest a proxied HLS playback hands to the client must address every
// segment back through this proxy: proxy-relative paths, virtual item id, proxy
// token — and no upstream host anywhere, even when a streamingUrl distinct from
// the API address is configured.
func TestHLSManifestRoutesSegmentsThroughProxyWithStreamingURL(t *testing.T) {
	upstream, _ := streamingUpstream(t)
	config := configWithStreamingURLs(upstream.URL, upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		client := app.Upstream.GetClient(0)
		virtual := app.IDStore.GetOrCreateVirtualID("episode-1", client.ID)

		req := httptest.NewRequest(http.MethodGet, "/Videos/"+virtual+"/master.m3u8?api_key="+token, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		want := "/Videos/" + virtual + "/segment1.ts?api_key=" + token
		if !strings.Contains(body, want) {
			t.Fatalf("manifest missing proxy-relative segment %q: %s", want, body)
		}
		if strings.Contains(body, "127.0.0.1") {
			t.Fatalf("a host survived into the client-facing manifest: %s", body)
		}

		// The rewritten segment must actually play through the proxy.
		segment := httptest.NewRequest(http.MethodGet, want, nil)
		segmentRR := httptest.NewRecorder()
		handler.ServeHTTP(segmentRR, segment)
		if segmentRR.Code != http.StatusOK || segmentRR.Body.String() != "segment-body" {
			t.Fatalf("segment via proxy: status=%d body=%q", segmentRR.Code, segmentRR.Body.String())
		}
	})
}

// The ordered list survives a save/load round trip, the legacy single-value key
// folds into the list, and duplicates and blanks are dropped.
func TestStreamingURLsConfigRoundTrip(t *testing.T) {
	cfg, err := parseConfigYAML("server:\n  port: 8096\nadmin:\n  username: a\n  password: b\nplayback:\n  mode: proxy\nupstream:\n  - name: A\n    url: 'http://api.example'\n    username: u\n    password: p\n    streamingUrl: 'http://legacy.example/'\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	normalizeUpstream(&cfg.Upstream[0], 0, cfg)
	if len(cfg.Upstream) != 1 || len(cfg.Upstream[0].StreamingURLs) != 1 || cfg.Upstream[0].StreamingURLs[0] != "http://legacy.example" {
		t.Fatalf("legacy key not migrated: %+v", cfg.Upstream[0].StreamingURLs)
	}

	cfg2, err := parseConfigYAML("server:\n  port: 8096\nadmin:\n  username: a\n  password: b\nplayback:\n  mode: proxy\nupstream:\n  - name: A\n    url: 'http://api.example'\n    username: u\n    password: p\n    streamingUrls: ['http://a.example', 'http://a.example', '', 'http://b.example/']\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	normalizeUpstream(&cfg2.Upstream[0], 0, cfg2)
	want := []string{"http://a.example", "http://b.example"}
	if fmt.Sprint(cfg2.Upstream[0].StreamingURLs) != fmt.Sprint(want) {
		t.Fatalf("normalized list = %v, want %v", cfg2.Upstream[0].StreamingURLs, want)
	}

	rendered := renderConfigYAML(cfg2)
	if !strings.Contains(rendered, "streamingUrls: ['http://a.example', 'http://b.example']") {
		t.Fatalf("rendered config lost the ordered list:\n%s", rendered)
	}
	reloaded, err := parseConfigYAML(rendered)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if fmt.Sprint(reloaded.Upstream[0].StreamingURLs) != fmt.Sprint(want) {
		t.Fatalf("round-trip list = %v, want %v", reloaded.Upstream[0].StreamingURLs, want)
	}
}
