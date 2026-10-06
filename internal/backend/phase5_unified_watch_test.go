package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type unifiedWatchUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	hook   func(*http.Request, map[string]any)
}
type unifiedWatchFixture struct {
	app      *App
	handler  http.Handler
	token    string
	info     *tokenInfo
	item     string
	media    [2]string
	sessions [2]string
	upstream [2]*unifiedWatchUpstream
}

func newUnifiedWatchUpstream(t *testing.T, label string) *unifiedWatchUpstream {
	t.Helper()
	u := &unifiedWatchUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "up-" + label, "User": map[string]any{"Id": "user-" + label}})
		case r.URL.Path == "/Items/Counts":
			_ = json.NewEncoder(w).Encode(map[string]any{"MovieCount": 1, "SeriesCount": 2, "EpisodeCount": 3})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "source-" + label})
		case r.URL.Path == "/Users/user-"+label+"/Items/item-"+label || r.URL.Path == "/Items/item-"+label:
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "item-" + label, "Type": "Movie", "Name": "Confirmed merged movie", "RunTimeTicks": 1000,
				"MediaSources": []any{map[string]any{"Id": "ms-" + label, "ItemId": "item-" + label, "RunTimeTicks": 1000, "Container": "mp4"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-"+label+"/PlayedItems/item-"+label:
			_ = json.NewEncoder(w).Encode(map[string]any{"Played": true, "PlaybackPositionTicks": 0})
		case r.Method == http.MethodPost && (r.URL.Path == "/Sessions/Playing" || r.URL.Path == "/Sessions/Playing/Progress" || r.URL.Path == "/Sessions/Playing/Stopped"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid test body", 400)
				return
			}
			u.mu.Lock()
			hook := u.hook
			u.mu.Unlock()
			if hook != nil {
				hook(r, body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}
func withUnifiedWatchFixture(t *testing.T, fn func(*unifiedWatchFixture)) {
	t.Helper()
	a, b := newUnifiedWatchUpstream(t, "a"), newUnifiedWatchUpstream(t, "b")
	withTempAppConfig(t, strings.ReplaceAll(phase1EDualPlaybackConfig(a.server.URL, b.server.URL), "maxConcurrent: 1", "maxConcurrent: 0"), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("issued user missing")
		}
		f := &unifiedWatchFixture{app: app, handler: handler, token: token, info: info, upstream: [2]*unifiedWatchUpstream{a, b}}
		f.item = app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		app.IDStore.AssociateAdditionalInstance(f.item, "item-b", "server-b")
		for i, label := range []string{"a", "b"} {
			f.media[i] = app.IDStore.GetOrCreateVirtualID("ms-"+label, "server-"+label)
			f.sessions[i] = app.IDStore.GetOrCreateVirtualID("play-"+label, "server-"+label)
		}
		fn(f)
	})
}
func (f *unifiedWatchFixture) body(source int, position int64) map[string]any {
	return map[string]any{"ItemId": f.item, "MediaSourceId": f.media[source], "PlaySessionId": f.sessions[source], "PositionTicks": position, "RunTimeTicks": int64(1000)}
}
func (f *unifiedWatchFixture) send(t *testing.T, source int, path string, position int64) {
	t.Helper()
	rr := phase1ESessionPost(t, f.handler, path, f.token, "real-device", f.body(source, position))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
	}
}
func (f *unifiedWatchFixture) assertProgress(t *testing.T, position int64, played bool, server string) {
	t.Helper()
	p := f.app.WatchStore.GetProgress(f.info.UserID, f.item)
	if p == nil || p.PositionTicks != position || p.Played != played || p.ServerID != server {
		t.Fatalf("shared progress=%+v want position=%d played=%v server=%s", p, position, played, server)
	}
}
func (f *unifiedWatchFixture) gateProgress(t *testing.T) (<-chan struct{}, func(), <-chan struct{}) {
	t.Helper()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f.upstream[0].mu.Lock()
	f.upstream[0].hook = func(r *http.Request, body map[string]any) {
		if r.URL.Path != "/Sessions/Playing/Progress" {
			return
		}
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	f.upstream[0].mu.Unlock()
	raw, err := json.Marshal(f.body(0, 800))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Progress", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", f.token)
	req.Header.Set("X-Emby-Device-Id", "real-device")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	go func() { defer close(done); f.handler.ServeHTTP(rr, req) }()
	t.Cleanup(func() {
		unblock()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("gated request did not finish")
		}
	})
	select {
	case <-entered:
	case <-done:
		t.Fatalf("request missed gate: %d %s", rr.Code, rr.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("gate timeout")
	}
	return entered, unblock, done
}
func waitUnifiedRequest(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request completion timeout")
	}
}
func TestUnifiedCompletionThresholdAndStorePreservation(t *testing.T) {
	for _, tc := range []struct {
		name, itemType       string
		position, runtime    int64
		kind                 playbackWatchEventKind
		live, failed, played bool
	}{
		{"below", "Movie", 899, 1000, playbackWatchProgress, false, false, false},
		{"exact", "Movie", 900, 1000, playbackWatchProgress, false, false, true},
		{"above", "Episode", 950, 1000, playbackWatchStopped, false, false, true},
		{"audio", "Audio", 950, 1000, playbackWatchProgress, false, false, false},
		{"live", "Movie", 950, 1000, playbackWatchProgress, true, false, false},
		{"failed stop", "Movie", 950, 1000, playbackWatchStopped, false, true, false},
		{"started", "Movie", 950, 1000, playbackWatchStarted, false, false, false},
		{"unknown runtime", "Movie", 950, 0, playbackWatchProgress, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newTestWatchStore(t)
			meta := WatchProgress{ProxyUserID: "u", VirtualItemID: "v", ServerID: "a", OriginalItemID: "item", ItemType: tc.itemType}
			if err := ws.RecordProgress(&meta); err != nil {
				t.Fatal(err)
			}
			if err := ws.SetFavorite("u", "v", true); err != nil {
				t.Fatal(err)
			}
			event := playbackWatchEvent{Kind: tc.kind, Source: playbackWatchSource{ServerID: "a", OriginalItemID: "item", MediaSourceID: "ms"}, Position: validPlaybackTicks(tc.position), Runtime: validPlaybackTicks(tc.runtime), Live: tc.live, Failed: tc.failed}
			if err := ws.RecordPlaybackProgress(&meta, event); err != nil {
				t.Fatal(err)
			}
			p := ws.GetProgress("u", "v")
			want := tc.position
			if tc.played {
				want = 0
			}
			if p == nil || p.Played != tc.played || p.PositionTicks != want || !p.IsFavorite {
				t.Fatalf("result=%+v", p)
			}
			if tc.played {
				event.Position = validPlaybackTicks(100)
				if err := ws.RecordPlaybackProgress(&meta, event); err != nil {
					t.Fatal(err)
				}
				p = ws.GetProgress("u", "v")
				if !p.Played || p.PositionTicks != 0 || !p.IsFavorite {
					t.Fatalf("completion regressed: %+v", p)
				}
			}
		})
	}
}
func TestUnifiedSharedOwnerAndLateResponse(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 400)
		_, release, done := f.gateProgress(t)
		f.send(t, 1, "/Sessions/Playing", 0)
		f.send(t, 1, "/Sessions/Playing/Progress", 300)
		release()
		waitUnifiedRequest(t, done)
		f.assertProgress(t, 300, false, "server-b")
		f.send(t, 0, "/Sessions/Playing/Stopped", 850)
		f.assertProgress(t, 300, false, "server-b")
		f.send(t, 1, "/Sessions/Playing/Progress", 900)
		f.assertProgress(t, 0, true, "server-b")
	})
}
func TestUnifiedManualResetFencesInFlightProgress(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 400)
		_, release, done := f.gateProgress(t)
		path := "/Users/" + f.app.Auth.ProxyUserID() + "/PlayedItems/" + f.item
		rr := doAuthJSON(t, f.handler, http.MethodPost, path, nil, f.token)
		if rr.Code != http.StatusOK {
			t.Fatalf("manual played: %d %s", rr.Code, rr.Body.String())
		}
		release()
		waitUnifiedRequest(t, done)
		f.assertProgress(t, 0, true, "server-a")
	})
}
func TestUnifiedUnbindingRetainsHistoryAndRequiresFreshAuthorization(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 400)
		orphan := f.app.IDStore.GetOrCreateVirtualID("only-a", "server-a")
		if err := f.app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: f.info.UserID, VirtualItemID: orphan, ServerID: "server-a", OriginalItemID: "only-a", PositionTicks: 250}); err != nil {
			t.Fatal(err)
		}
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		rr := doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{"allowedServers": []string{"server-b"}}, admin)
		if rr.Code != 200 {
			t.Fatalf("unbind: %d %s", rr.Code, rr.Body.String())
		}
		if rr = doAuthJSON(t, f.handler, http.MethodGet, "/Items/"+f.item, nil, f.token); rr.Code != 401 {
			t.Fatalf("old token status=%d", rr.Code)
		}
		f.token = loginTokenAs(t, f.handler, "alice", "password123")
		f.info = f.app.Auth.ValidateToken(f.token)
		reqCtx := &RequestContext{ProxyToken: f.token, ProxyUser: f.info}
		var merged, hidden *WatchProgress
		if err := f.app.withVisibleWatchScope(reqCtx, false, func(scope mediaAccessScope) error {
			var err error
			merged, err = f.app.WatchStore.GetVisibleProgress(scope, f.item)
			if err != nil {
				return err
			}
			hidden, err = f.app.WatchStore.GetVisibleProgress(scope, orphan)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if merged == nil || merged.PositionTicks != 400 || hidden != nil || f.app.WatchStore.GetProgress(f.info.UserID, orphan) == nil {
			t.Fatalf("visibility merged=%+v hidden=%+v", merged, hidden)
		}
		rr = doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{"allowedServers": []string{"server-a", "server-b"}}, admin)
		if rr.Code != 200 {
			t.Fatalf("rebind: %d %s", rr.Code, rr.Body.String())
		}
		f.token = loginTokenAs(t, f.handler, "alice", "password123")
		f.info = f.app.Auth.ValidateToken(f.token)
		reqCtx = &RequestContext{ProxyToken: f.token, ProxyUser: f.info}
		if err := f.app.withVisibleWatchScope(reqCtx, false, func(scope mediaAccessScope) error {
			var err error
			hidden, err = f.app.WatchStore.GetVisibleProgress(scope, orphan)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if hidden == nil || hidden.PositionTicks != 250 {
			t.Fatalf("rebound history=%+v", hidden)
		}
	})
}
func TestUnifiedUnchangedGrantKeepsTokenAndRevision(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		before := f.info.AuthRevision
		rr := doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{"enabled": true, "allowedServers": []string{"server-b", "server-a", "server-a"}}, admin)
		if rr.Code != 200 || !f.app.Auth.HasIssuedToken(f.token) || f.app.UserStore.Get(f.info.UserID).AuthRevision != before {
			t.Fatalf("no-op update: %d %s", rr.Code, rr.Body.String())
		}
	})
}
func TestUnifiedSQLCleanupFailureRecoversAfterConfigurationCommit(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 400)
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		db := f.app.IDStore.db
		if err := db.writeParams("CREATE TRIGGER unified_cleanup_failure BEFORE DELETE ON user_servers BEGIN SELECT RAISE(ABORT,'isolated cleanup fault'); END"); err != nil {
			t.Fatal(err)
		}
		rr := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-a", nil, admin)
		if rr.Code != 500 || !f.app.lifecyclePending || configuredSourceIDs(f.app.ConfigStore.Snapshot())["server-a"] {
			t.Fatalf("committed config failure: %d %s", rr.Code, rr.Body.String())
		}
		ops, err := f.app.pendingCleanupOperations()
		if err != nil || len(ops) != 1 || ops[0].Phase != "prepared" {
			t.Fatalf("journal=%+v err=%v", ops, err)
		}
		f.assertProgress(t, 400, false, "server-a")
		if err := db.writeParams("DROP TRIGGER unified_cleanup_failure"); err != nil {
			t.Fatal(err)
		}
		rr = doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{}, admin)
		if rr.Code != 200 || f.app.lifecyclePending {
			t.Fatalf("recovery: %d %s", rr.Code, rr.Body.String())
		}
		f.assertProgress(t, 400, false, "server-b")
		ops, err = f.app.pendingCleanupOperations()
		if err != nil || len(ops) != 0 {
			t.Fatalf("pending after recovery: %+v %v", ops, err)
		}
		if f.app.UserStore.Get(f.info.UserID).AuthRevision != f.info.AuthRevision+1 {
			t.Fatal("cleanup revision repeated")
		}
	})
}
func TestUnifiedTokenPersistenceFailureStaysPendingAndRecoversOnce(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		oldPath := f.app.Auth.tokenFile
		f.app.Auth.saveMu.Lock()
		f.app.Auth.tokenFile = t.TempDir()
		f.app.Auth.saveMu.Unlock()
		t.Cleanup(func() { f.app.Auth.saveMu.Lock(); f.app.Auth.tokenFile = oldPath; f.app.Auth.saveMu.Unlock() })
		rr := doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{"allowedServers": []string{"server-b"}}, admin)
		if rr.Code != 500 || !f.app.lifecyclePending {
			t.Fatalf("token persistence failure: %d %s", rr.Code, rr.Body.String())
		}
		if f.app.Auth.ValidateToken(f.token) != nil {
			t.Fatal("stale authorization survived durable revision")
		}
		ops, err := f.app.pendingCleanupOperations()
		if err != nil || len(ops) != 1 || ops[0].Phase != "committed" {
			t.Fatalf("journal=%+v %v", ops, err)
		}
		f.app.Auth.saveMu.Lock()
		f.app.Auth.tokenFile = oldPath
		f.app.Auth.saveMu.Unlock()
		rr = doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{}, admin)
		if rr.Code != 200 || f.app.lifecyclePending {
			t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
		}
		if f.app.UserStore.Get(f.info.UserID).AuthRevision != f.info.AuthRevision+1 {
			t.Fatal("recovery incremented revision twice")
		}
		ops, err = f.app.pendingCleanupOperations()
		if err != nil || len(ops) != 0 {
			t.Fatalf("journal after retry=%+v %v", ops, err)
		}
	})
}

func TestUnifiedAdmittedProgressSurvivesSourceDeletionWithoutReusingRuntime(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 400)
		_, release, done := f.gateProgress(t)
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		rr := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-a", nil, admin)
		if rr.Code != 200 {
			t.Fatalf("delete while admitted: %d %s", rr.Code, rr.Body.String())
		}
		f.assertProgress(t, 400, false, "server-b")
		release()
		waitUnifiedRequest(t, done)
		f.assertProgress(t, 800, false, "server-b")
		p := f.app.WatchStore.GetProgress(f.info.UserID, f.item)
		if p.RuntimeTicks != 0 {
			t.Fatalf("A runtime mislabeled as B: %+v", p)
		}
		rr = doAuthJSON(t, f.handler, http.MethodPost, "/Sessions/Playing/Progress", f.body(1, 900), f.token)
		if rr.Code != 401 {
			t.Fatalf("new request with revoked token status=%d", rr.Code)
		}
	})
}
func TestUnifiedDeletingUserCannotRecreateHistoryFromAdmittedProgress(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 400)
		_, release, done := f.gateProgress(t)
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		rr := doAuthJSON(t, f.handler, http.MethodPost, "/admin/api/users", map[string]any{"username": "bob", "password": "password123", "allowedServers": []string{"server-a", "server-b"}}, admin)
		if rr.Code != 201 {
			t.Fatalf("other user: %d %s", rr.Code, rr.Body.String())
		}
		bob := loginTokenAs(t, f.handler, "bob", "password123")
		bobInfo := f.app.Auth.ValidateToken(bob)
		if err := f.app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: bobInfo.UserID, VirtualItemID: f.item, ServerID: "server-b", OriginalItemID: "item-b", PositionTicks: 555, IsFavorite: true}); err != nil {
			t.Fatal(err)
		}
		rr = doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/users/"+f.info.UserID, nil, admin)
		if rr.Code != 200 {
			t.Fatalf("delete user: %d %s", rr.Code, rr.Body.String())
		}
		release()
		waitUnifiedRequest(t, done)
		if f.app.WatchStore.GetProgress(f.info.UserID, f.item) != nil || f.app.Auth.ValidateToken(f.token) != nil {
			t.Fatal("deleted user/history recreated")
		}
		p := f.app.WatchStore.GetProgress(bobInfo.UserID, f.item)
		if p == nil || p.PositionTicks != 555 || !p.IsFavorite || f.app.Auth.ValidateToken(bob) == nil {
			t.Fatalf("other user changed: %+v", p)
		}
	})
}

func TestUnifiedMissingOwnershipAndVersionProofCannotCreateHistory(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing/Progress", 900)
		if f.app.WatchStore.GetProgress(f.info.UserID, f.item) != nil {
			t.Fatal("bare Progress created a writer")
		}
		other := f.app.IDStore.GetOrCreateVirtualID("not-item-version", "server-a")
		body := f.body(0, 950)
		body["MediaSourceId"] = other
		rr := phase1ESessionPost(t, f.handler, "/Sessions/Playing", f.token, "real-device", body)
		if rr.Code != 204 {
			t.Fatalf("control response: %d %s", rr.Code, rr.Body.String())
		}
		if f.app.WatchStore.GetProgress(f.info.UserID, f.item) != nil {
			t.Fatal("unproven version created history")
		}
	})
}
func TestUnifiedProvisionalRevisionAndExpiredCleanup(t *testing.T) {
	limiter := NewPlaybackLimiter()
	lease := limiter.Reserve("u", "s", "d", "i", "")
	if !lease.Allowed {
		t.Fatal("provisional reserve")
	}
	revision := limiter.PlaybackRevision("u", "s", "d", "i", "")
	if revision == 0 || !limiter.StopPlaybackRevision("u", "s", "d", "i", "", revision) {
		t.Fatal("exact provisional revision did not stop")
	}
	limiter.Reserve("u", "s", "d", "i", "current")
	if limiter.PlaybackRevision("u", "s", "d", "i", "") != 0 {
		t.Fatal("empty session matched committed lease")
	}
	limiter.mu.Lock()
	limiter.streams[streamKey{UserID: "u", ServerID: "s"}].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
	limiter.mu.Unlock()
	if limiter.PlaybackRevision("u", "s", "wrong", "i", "old") != 0 {
		t.Fatal("expired revision returned")
	}
	limiter.mu.Lock()
	remaining := len(limiter.streams)
	limiter.mu.Unlock()
	if remaining != 0 {
		t.Fatal("revision capture did not clean expired lease")
	}
}
