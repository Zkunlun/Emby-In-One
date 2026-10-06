package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const phase7KDeviceID = "shared-device"
const phase7KSessionID = "shared-session"
const phase7KItemID = "shared-item"

type phase7KForwardedSession struct {
	Path string
	Body map[string]any
}

type phase7KUpstream struct {
	Server   *httptest.Server
	mu       sync.Mutex
	status   int
	requests []phase7KForwardedSession
	hook     func(*http.Request, map[string]any, int) int
}

func phase7KNewUpstream(t *testing.T, serverID string) *phase7KUpstream {
	t.Helper()
	u := &phase7KUpstream{status: http.StatusNoContent}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "upstream-token-" + serverID,
				"User":        map[string]any{"Id": "upstream-user-" + serverID},
			})
		case r.Method == http.MethodPost && (r.URL.Path == "/Sessions/Playing" || r.URL.Path == "/Sessions/Playing/Progress" || r.URL.Path == "/Sessions/Playing/Stopped"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid session body", http.StatusBadRequest)
				return
			}
			u.mu.Lock()
			u.requests = append(u.requests, phase7KForwardedSession{Path: r.URL.Path, Body: body})
			status, hook := u.status, u.hook
			u.mu.Unlock()
			if hook != nil {
				status = hook(r, body, status)
			}
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Server.Close)
	return u
}

func (u *phase7KUpstream) Requests() []phase7KForwardedSession {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]phase7KForwardedSession(nil), u.requests...)
}

type phase7KUser struct {
	Token string
	Info  *tokenInfo
}

type phase7KFixture struct {
	App        *App
	Handler    http.Handler
	AdminToken string
	Users      [2]phase7KUser
	Servers    [2]string
	Upstreams  [2]*phase7KUpstream
	Items      [2]string
	Media      [2]string
	Sessions   [2]string
}

func phase7KCreateUser(t *testing.T, f *phase7KFixture, name string, allowed []string) phase7KUser {
	t.Helper()
	rr := doAuthJSON(t, f.Handler, http.MethodPost, "/admin/api/users", map[string]any{
		"username": name, "password": "password123", "allowedServers": allowed,
	}, f.AdminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", name, rr.Code, rr.Body.String())
	}
	token := loginTokenAs(t, f.Handler, name, "password123")
	info := f.App.Auth.ValidateToken(token)
	if info == nil || info.Role == "admin" {
		t.Fatalf("invalid regular user for %s", name)
	}
	return phase7KUser{Token: token, Info: info}
}

func phase7KWithFixture(t *testing.T, fn func(*phase7KFixture)) {
	t.Helper()
	upstreams := [2]*phase7KUpstream{phase7KNewUpstream(t, "server-a"), phase7KNewUpstream(t, "server-b")}
	// Capacity is unrelated to isolation; leave room for boundary-only users.
	config := strings.ReplaceAll(phase1EDualPlaybackConfig(upstreams[0].Server.URL, upstreams[1].Server.URL), "maxConcurrent: 1", "maxConcurrent: 0")
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		f := &phase7KFixture{App: app, Handler: handler, Servers: [2]string{"server-a", "server-b"}, Upstreams: upstreams}
		f.AdminToken = loginTokenAs(t, handler, "admin", "secret")
		for userIndex, name := range []string{"phase7k-alice", "phase7k-bob"} {
			f.Users[userIndex] = phase7KCreateUser(t, f, name, f.Servers[:])
		}
		if f.Users[0].Token == f.Users[1].Token || f.Users[0].Info.UserID == f.Users[1].Info.UserID {
			t.Fatal("two real users must have distinct tokens and identities")
		}
		for serverIndex, serverID := range f.Servers {
			// Original IDs deliberately collide across both upstreams.
			f.Items[serverIndex] = app.IDStore.GetOrCreateVirtualID(phase7KItemID, serverID)
			f.Media[serverIndex] = app.IDStore.GetOrCreateVirtualID("shared-media", serverID)
			f.Sessions[serverIndex] = app.IDStore.GetOrCreateVirtualID(phase7KSessionID, serverID)
			for userIndex, user := range f.Users {
				if err := app.WatchStore.RecordProgress(&WatchProgress{
					ProxyUserID: user.Info.UserID, VirtualItemID: f.Items[serverIndex],
					ServerID: serverID, OriginalItemID: phase7KItemID, ItemType: "Movie",
					Name: "fixture movie", PositionTicks: int64(100 + userIndex*100 + serverIndex*10),
					RuntimeTicks: 1000, IsFavorite: true, LastPlayed: 1, UpdatedAt: 1,
				}); err != nil {
					t.Fatalf("seed progress: %v", err)
				}
				seedPhase5WatchOwner(t, app, user.Info.UserID, f.Items[serverIndex], serverID, phase7KItemID, phase7KDeviceID, phase7KSessionID, "shared-media", 1000)
				lease := app.PlaybackLimiter.Reserve(user.Info.UserID, serverID, phase7KDeviceID, f.Items[serverIndex], phase7KSessionID)
				if !lease.Allowed || !lease.Created {
					t.Fatalf("seed lease user=%d server=%s result=%+v", userIndex, serverID, lease)
				}
				phase1EAgeLease(t, app, user.Info.UserID, serverID, time.Now().Add(-time.Minute))
				owner := "token:" + user.Token
				app.playbackRoutes.Activate(owner, f.Items[serverIndex], serverID, phase7KSessionID, f.Sessions[serverIndex])
				app.playbackRoutes.RememberMediaSource(owner, f.Media[serverIndex], serverID, phase7KSessionID, f.Sessions[serverIndex])
				app.playbackRoutes.RememberMediaSourceItem(owner, f.Media[serverIndex], f.Items[serverIndex], serverID)
			}
		}
		if f.Items[0] == f.Items[1] || f.Sessions[0] == f.Sessions[1] {
			t.Fatal("unmerged original IDs must retain upstream namespaces")
		}
		fn(f)
	})
}

func (f *phase7KFixture) Key(userIndex, serverIndex int) streamKey {
	return streamKey{UserID: f.Users[userIndex].Info.UserID, ServerID: f.Servers[serverIndex]}
}

func (f *phase7KFixture) Body(userIndex, serverIndex int, position int64) map[string]any {
	return map[string]any{
		"ItemId": f.Items[serverIndex], "MediaSourceId": f.Media[serverIndex],
		"PlaySessionId": f.Sessions[serverIndex], "UserId": f.Users[userIndex].Info.UserID,
		"PositionTicks": position, "RunTimeTicks": int64(2000),
	}
}

type phase7KSnapshot struct {
	Progress map[streamKey]WatchProgress
	Leases   map[streamKey]phase7ILeaseSnapshot
	Active   map[playbackRouteKey]playbackRouteEntry
	Media    map[playbackMediaRouteKey]playbackRouteEntry
}

func (f *phase7KFixture) Snapshot(t *testing.T) phase7KSnapshot {
	t.Helper()
	s := phase7KSnapshot{
		Progress: map[streamKey]WatchProgress{}, Leases: map[streamKey]phase7ILeaseSnapshot{},
		Active: map[playbackRouteKey]playbackRouteEntry{}, Media: map[playbackMediaRouteKey]playbackRouteEntry{},
	}
	for userIndex, user := range f.Users {
		for serverIndex := range f.Servers {
			p := f.App.WatchStore.GetProgress(user.Info.UserID, f.Items[serverIndex])
			if p == nil {
				t.Fatalf("fixture watch row missing user=%d server=%d", userIndex, serverIndex)
			}
			s.Progress[f.Key(userIndex, serverIndex)] = *p
		}
	}
	f.App.PlaybackLimiter.mu.Lock()
	for key, entry := range f.App.PlaybackLimiter.streams {
		s.Leases[key] = phase7ILeaseSnapshot{DeviceID: entry.DeviceID, ItemID: entry.ItemID, PlaySessionID: entry.PlaySessionID, Revision: entry.Revision, LastHeartbeat: entry.LastHeartbeat.UnixNano()}
	}
	f.App.PlaybackLimiter.mu.Unlock()
	f.App.playbackRoutes.mu.RLock()
	for key, entry := range f.App.playbackRoutes.active {
		s.Active[key] = entry
	}
	for key, entry := range f.App.playbackRoutes.mediaSource {
		s.Media[key] = entry
	}
	f.App.playbackRoutes.mu.RUnlock()
	return s
}

func (f *phase7KFixture) AssertUnchangedExcept(t *testing.T, before phase7KSnapshot, changed ...streamKey) {
	t.Helper()
	after := f.Snapshot(t)
	except := map[streamKey]bool{}
	for _, key := range changed {
		except[key] = true
	}
	for key, progress := range before.Progress {
		if !except[key] && after.Progress[key] != progress {
			t.Fatalf("unrelated watch row changed key=%+v before=%+v after=%+v", key, progress, after.Progress[key])
		}
	}
	for key, lease := range before.Leases {
		if except[key] {
			continue
		}
		if current, ok := after.Leases[key]; !ok || current != lease {
			t.Fatalf("unrelated lease changed key=%+v before=%+v after=%+v exists=%v", key, lease, current, ok)
		}
	}
	for key := range after.Leases {
		if _, ok := before.Leases[key]; !ok {
			t.Fatalf("unexpected lease created: %+v", key)
		}
	}
	if !reflect.DeepEqual(after.Active, before.Active) || !reflect.DeepEqual(after.Media, before.Media) {
		t.Fatal("lifecycle request modified owner-scoped playback routes")
	}
}

func (f *phase7KFixture) AssertCommitted(t *testing.T, before phase7KSnapshot, userIndex, serverIndex int, position int64, stopped bool) {
	t.Helper()
	key := f.Key(userIndex, serverIndex)
	p := f.App.WatchStore.GetProgress(key.UserID, f.Items[serverIndex])
	if p == nil || p.LastPlayed <= before.Progress[key].LastPlayed || p.UpdatedAt <= before.Progress[key].UpdatedAt {
		t.Fatalf("confirmed event did not advance timestamps: %+v", p)
	}
	want := before.Progress[key]
	want.PositionTicks, want.RuntimeTicks = position, 2000
	want.LastPlayed, want.UpdatedAt = p.LastPlayed, p.UpdatedAt
	if *p != want {
		t.Fatalf("event committed incorrect watch state: want=%+v got=%+v", want, *p)
	}
	f.App.PlaybackLimiter.mu.Lock()
	_, exists := f.App.PlaybackLimiter.streams[key]
	f.App.PlaybackLimiter.mu.Unlock()
	if stopped {
		if exists {
			t.Fatalf("matching stopped lease still exists: %+v", key)
		}
		return
	}
	if !exists {
		t.Fatalf("confirmed event removed lease: %+v", key)
	}
	after := phase7ISnapshotLease(t, f.App, key.UserID, key.ServerID)
	wantLease := before.Leases[key]
	if after.LastHeartbeat <= wantLease.LastHeartbeat {
		t.Fatalf("target heartbeat did not advance: before=%+v after=%+v", wantLease, after)
	}
	wantLease.LastHeartbeat = after.LastHeartbeat
	if after != wantLease {
		t.Fatalf("heartbeat changed lease ownership: want=%+v got=%+v", wantLease, after)
	}
}

func phase7KAssertEmptySuccess(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q want empty 204", rr.Code, rr.Body.String())
	}
}

func (f *phase7KFixture) AssertForwarded(t *testing.T, serverIndex int, path string, position int64) {
	t.Helper()
	requests := f.Upstreams[serverIndex].Requests()
	if len(requests) == 0 {
		t.Fatal("session request was not forwarded")
	}
	sent := requests[len(requests)-1]
	gotPosition, _ := numericInt64(sent.Body["PositionTicks"])
	if sent.Path != path || gotPosition != position || sent.Body["ItemId"] != phase7KItemID ||
		sent.Body["PlaySessionId"] != phase7KSessionID || sent.Body["UserId"] != "upstream-user-"+f.Servers[serverIndex] {
		t.Fatalf("wrong upstream session identity/body: %+v", sent)
	}
}
