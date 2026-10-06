package backend

// Phase4 prepares these fixtures; tests remain NOT RUN until Phase5.
// Everything below uses synthetic in-memory users/configuration and an injected
// RoundTripper. No fixture opens a database, logs in, or dials an upstream.
import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type countsTestCollectorFunc func(context.Context, countsSourceSpec) countsRoundResult

func (f countsTestCollectorFunc) collect(ctx context.Context, source countsSourceSpec) countsRoundResult {
	return f(ctx, source)
}

type countsTestTransportFunc func(*http.Request) (*http.Response, error)

func (f countsTestTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func countsTestClient(id string) *UpstreamClient {
	cfg := UpstreamConfig{ID: id, Name: id, URL: "https://counts.invalid/" + id, Username: "fixture-account", SpoofClient: "none"}
	client := &UpstreamClient{ID: id, Name: id, BaseURL: cfg.URL, Config: cfg, Online: true, UserID: "real-" + id, AccessToken: "fixture-upstream-token"}
	client.countsLoginConfig = client.configuredCountsLoginBinding()
	return client
}

func countsTestCache(t *testing.T, now func() time.Time, collector countsCollector) *countsCache {
	t.Helper()
	cache, err := newCountsCache(now, collector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.close)
	return cache
}

func countsTestApp(t *testing.T) (*App, *atomic.Int32) {
	t.Helper()
	cfg := &Config{}
	clients := []*UpstreamClient{countsTestClient("a"), countsTestClient("b")}
	var network atomic.Int32
	transport := countsTestTransportFunc(func(*http.Request) (*http.Response, error) {
		network.Add(1)
		return nil, errors.New("fixture forbids unexpected transport")
	})
	for _, client := range clients {
		client.httpClient = &http.Client{Transport: transport}
		client.transport = transport
		cfg.Upstream = append(cfg.Upstream, client.Config)
	}
	user := &User{ID: "alice", Username: "alice", Enabled: true, AllowedServers: []string{"a", "b"}, AuthRevision: 7}
	users := &UserStore{users: map[string]*User{"alice": user}}
	auth := &AuthManager{users: users, proxyUserID: "legacy", tokens: map[string]tokenInfo{
		"fixture-local-token": {UserID: "alice", Role: "user", AllowedServers: []string{"a", "b"}, AuthRevision: 7},
		"fixture-admin-token": {UserID: "admin", Role: "admin"},
	}}
	app := &App{ConfigStore: &ConfigStore{config: cfg}, UserStore: users, Auth: auth, Upstream: &UpstreamPool{clients: clients}}
	var collections atomic.Int32
	cache := countsTestCache(t, func() time.Time { return time.Unix(1700000000, 0) }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		collections.Add(1)
		return countsRoundResult{Class: countsTransport}
	}))
	app.mediaCounts = &mediaCountsService{app: app, cache: cache, wake: make(chan struct{}, 1)}
	for i, client := range clients {
		cache.syncSource(cache.prepareSource(client, nil, ""))
		state := cache.states[client.ID]
		state.hasSnapshot = true
		state.snapshot = countsSnapshot{Value: mediaCounts{int64(10 * (i + 1)), int64(i + 1), int64(100 * (i + 1))}, DataGeneration: state.dataGeneration}
	}
	t.Cleanup(func() {
		if collections.Load() != 0 {
			t.Error("client reads invoked collector")
		}
		if network.Load() != 0 {
			t.Error("client reads invoked transport/login/probe/fallback")
		}
	})
	return app, &network
}

func countsTestRequest(handler http.Handler, method, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("X-Emby-Token", token)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func countsTestEventually(t *testing.T, condition func() bool) {
	t.Helper()
	// Wall timers are watchdogs for goroutine progress, never business-clock input.
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timeout.C:
			t.Fatal("fixture goroutine did not reach synchronization point")
		case <-poll.C:
		}
	}
}

func countsTestReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("fixture channel watchdog expired")
		var zero T
		return zero
	}
}
