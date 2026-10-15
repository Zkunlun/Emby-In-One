package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An unauthenticated request for an intentionally nonexistent stream ID
// cannot certify playback, even when a reverse proxy returns 200/403/404/500.
// It must never promote unknown/dead lines to verified alive.
func TestPhase8HAnonymousProbeCannotVerifyPlayback(t *testing.T) {
	const officialProbePath = "/Videos/00000000000000000000000000000000/stream"
	if streamLivenessProbePath != officialProbePath {
		t.Errorf("D12 must use an official Emby VideoService route shape, got %q", streamLivenessProbePath)
	}
	for _, tc := range []struct {
		status int
		name   string
	}{
		{http.StatusOK, "synthetic-200"}, {http.StatusUnauthorized, "401"},
		{http.StatusForbidden, "403"}, {http.StatusNotFound, "404"},
		{http.StatusInternalServerError, "500"},
	} {
		for _, initial := range []struct {
			name  string
			state streamBaseHealthState
		}{
			{"unknown", streamBaseUnknown}, {"dead", streamBaseDead}, {"alive", streamBaseAlive},
		} {
			t.Run(tc.name+"/"+initial.name, func(t *testing.T) {
				var sawProbe bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sawProbe = true
					if r.Method != http.MethodGet || r.URL.Path != officialProbePath {
						t.Errorf("D12 probe must be anonymous GET of Emby route shape, got %s %s", r.Method, r.URL.Path)
					}
					if r.URL.RawQuery != "" || r.Header.Get("X-Emby-Token") != "" ||
						r.Header.Get("Authorization") != "" || r.Header.Get("X-Emby-Authorization") != "" ||
						r.Header.Get("Range") != "" {
						t.Error("D12 liveness probe must carry no token, source, session, auth or range")
					}
					if r.Header.Get("User-Agent") != "Emby-In-One-Liveness/1.0" {
						t.Error("D12 probe sent an unexpected user agent")
					}
					w.WriteHeader(tc.status)
				}))
				defer server.Close()
				c := streamClientForBases(t, server.URL, server.URL, server.URL)
				if initial.state == streamBaseDead {
					c.markStreamBaseFailed(server.URL)
					c.mu.Lock()
					h := c.streamHealth[server.URL]
					h.lastFailure = time.Now().Add(-2 * streamFailureCooldown)
					c.streamHealth[server.URL] = h
					c.mu.Unlock()
				} else if initial.state == streamBaseAlive {
					c.markStreamBaseAlive(server.URL)
				}
				c.probeSelectedStreamBases(context.Background(), []string{server.URL}, time.Second)
				c.mu.RLock()
				got := c.streamHealth[server.URL].state
				c.mu.RUnlock()
				if !sawProbe || got != initial.state {
					t.Errorf("D12 anonymous %d liveness changed state %s -> %v; sawProbe=%t",
						tc.status, initial.name, got, sawProbe)
				}
			})
		}
	}
}

func TestPhase8HProbeGatewayFailureStillMarksDead(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		c := streamClientForBases(t, server.URL, server.URL, server.URL)
		c.probeSelectedStreamBases(context.Background(), []string{server.URL}, time.Second)
		c.mu.RLock()
		got := c.streamHealth[server.URL].state
		c.mu.RUnlock()
		server.Close()
		if got != streamBaseDead {
			t.Errorf("D12 gateway %d should be dead, got %v", status, got)
		}
	}
}

func TestPhase8HRealMediaRequestCanVerifyPlaybackAfterProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == streamLivenessProbePath {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/Videos/") {
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("sample"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	c := streamClientForBases(t, server.URL, server.URL)
	c.probeSelectedStreamBases(context.Background(), []string{server.URL}, time.Second)
	c.mu.RLock()
	state := c.streamHealth[server.URL].state
	c.mu.RUnlock()
	if state != streamBaseUnknown {
		t.Fatalf("D12 anonymous probe promoted %v to alive", state)
	}
	resp, err := c.Stream(context.Background(), nil, nil, "/Videos/valid/stream.mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c.mu.RLock()
	got := c.streamHealth[server.URL].state
	c.mu.RUnlock()
	if got != streamBaseAlive {
		t.Fatalf("D12 real media success did not mark alive: %v", got)
	}
}

// Media response headers from a real item request distinguish a successful
// stream from an HTTP error page. 404/401/403/500 must not certify Alive.
func TestPhase8HRealMediaHTTPFailureCannotVerifyPlayback(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := streamClientForBases(t, server.URL, server.URL)
		response, err := c.Stream(context.Background(), nil, nil, "/Videos/actual/stream", nil)
		if err != nil {
			t.Errorf("media HTTP %d should preserve original HTTP response: %v", code, err)
		}
		if response != nil {
			_ = response.Body.Close()
		}
		c.mu.RLock()
		state := c.streamHealth[server.URL].state
		c.mu.RUnlock()
		server.Close()
		if state != streamBaseUnknown {
			t.Errorf("D12 real media HTTP %d is not playback success: state=%v", code, state)
		}
	}
}
