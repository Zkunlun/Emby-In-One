package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

const phase7LStoppedPath = "/Sessions/Playing/Stopped"

type phase7LWatchKey struct {
	UserID string
	ItemID string
}

type phase7LState struct {
	Local phase7KSnapshot
	Watch map[phase7LWatchKey]WatchProgress
}

// Snapshots are taken at channel-controlled points where no handler is committing.
// Enumerate every row so unexpected progress for an admin, body UserId, or raw ID
// cannot hide outside the four fixture rows.
func phase7LSnapshot(t *testing.T, f *phase7KFixture) phase7LState {
	t.Helper()
	keys := func() []phase7LWatchKey {
		ws := f.App.WatchStore
		ws.mu.Lock()
		defer ws.mu.Unlock()
		stmt, err := ws.db.prepare("SELECT proxy_user_id, virtual_item_id FROM user_watch_progress")
		if err != nil {
			t.Fatal(err)
		}
		defer stmt.finalize()
		var keys []phase7LWatchKey
		for {
			row, err := stmt.step()
			if err != nil {
				t.Fatal(err)
			}
			if !row {
				break
			}
			keys = append(keys, phase7LWatchKey{UserID: stmt.columnText(0), ItemID: stmt.columnText(1)})
		}
		return keys
	}()
	state := phase7LState{Local: f.Snapshot(t), Watch: map[phase7LWatchKey]WatchProgress{}}
	for _, key := range keys {
		row := f.App.WatchStore.GetProgress(key.UserID, key.ItemID)
		if row == nil {
			t.Fatalf("snapshot watch row disappeared: %+v", key)
		}
		state.Watch[key] = *row
	}
	return state
}

func phase7LWatchOwner(f *phase7KFixture, user, server int) phase7LWatchKey {
	return phase7LWatchKey{UserID: f.Users[user].Info.UserID, ItemID: f.Items[server]}
}

func phase7LAssertUnchangedExcept(t *testing.T, f *phase7KFixture, before phase7LState, progress []phase7LWatchKey, leases []streamKey) {
	t.Helper()
	after := phase7LSnapshot(t, f)
	watchExcept, leaseExcept := map[phase7LWatchKey]bool{}, map[streamKey]bool{}
	for _, key := range progress {
		watchExcept[key] = true
	}
	for _, key := range leases {
		leaseExcept[key] = true
	}
	for key, want := range before.Watch {
		if watchExcept[key] {
			continue
		}
		if got, ok := after.Watch[key]; !ok || got != want {
			t.Fatalf("unrelated progress changed key=%+v want=%+v got=%+v exists=%v", key, want, got, ok)
		}
	}
	for key := range after.Watch {
		if _, ok := before.Watch[key]; !ok && !watchExcept[key] {
			t.Fatalf("unexpected progress created: %+v", key)
		}
	}
	for key, want := range before.Local.Leases {
		if leaseExcept[key] {
			continue
		}
		if got, ok := after.Local.Leases[key]; !ok || got != want {
			t.Fatalf("unrelated lease changed key=%+v want=%+v got=%+v exists=%v", key, want, got, ok)
		}
	}
	for key := range after.Local.Leases {
		if _, ok := before.Local.Leases[key]; !ok {
			t.Fatalf("Stopped created a lease: %+v", key)
		}
	}
	if !reflect.DeepEqual(after.Local.Active, before.Local.Active) || !reflect.DeepEqual(after.Local.Media, before.Local.Media) {
		t.Fatal("Stopped changed token-owned routes")
	}
}

func phase7LAssertProgress(t *testing.T, f *phase7KFixture, before phase7LState, key phase7LWatchKey, serverID, originalID string, position, runtime int64, played bool) {
	t.Helper()
	want, exists := before.Watch[key]
	if !exists {
		t.Fatalf("expected seeded progress: %+v", key)
	}
	row := f.App.WatchStore.GetProgress(key.UserID, key.ItemID)
	if row == nil || row.LastPlayed < want.LastPlayed || row.UpdatedAt < want.UpdatedAt || row.LastPlayed <= 1 || row.UpdatedAt <= 1 {
		t.Fatalf("Stopped did not persist final progress/timestamps: %+v", row)
	}
	want.ServerID, want.OriginalItemID = serverID, originalID
	want.PositionTicks, want.RuntimeTicks, want.Played = position, runtime, played
	want.LastPlayed, want.UpdatedAt = row.LastPlayed, row.UpdatedAt
	if *row != want {
		t.Fatalf("wrong final progress want=%+v got=%+v", want, *row)
	}
}

func phase7LAssertLeaseAbsent(t *testing.T, f *phase7KFixture, key streamKey) {
	t.Helper()
	f.App.PlaybackLimiter.mu.Lock()
	_, exists := f.App.PlaybackLimiter.streams[key]
	f.App.PlaybackLimiter.mu.Unlock()
	if exists {
		t.Fatalf("Stopped left lease: %+v", key)
	}
}

type phase7LMode struct {
	Name      string
	Status    int
	Transport error
}

func phase7LModes() []phase7LMode {
	return []phase7LMode{
		{Name: "HTTP 200", Status: 200}, {Name: "HTTP 201", Status: 201},
		{Name: "HTTP 202", Status: 202}, {Name: "HTTP 204", Status: 204},
		{Name: "HTTP 206", Status: 206}, {Name: "HTTP 299", Status: 299},
		{Name: "HTTP 300", Status: 300}, {Name: "HTTP 401", Status: 401},
		{Name: "HTTP 403", Status: 403}, {Name: "HTTP 429", Status: 429},
		{Name: "HTTP 500", Status: 500},
		{Name: "transport", Transport: errors.New("phase7l private transport detail")},
		{Name: "deadline", Transport: context.DeadlineExceeded},
		{Name: "net timeout", Transport: phase7ITimeoutError{}},
		{Name: "canceled", Transport: context.Canceled},
		{Name: "offline"}, {Name: "missing"},
		{Name: "auth preparation"}, {Name: "URL preparation"},
	}
}

func phase7LPrepareMode(t *testing.T, f *phase7KFixture, server int, mode phase7LMode) *atomic.Int32 {
	t.Helper()
	attempts := &atomic.Int32{}
	client := f.App.Upstream.ClientByID(f.Servers[server])
	if client == nil {
		t.Fatal("fixture client missing")
	}
	// Fault injection must not start an asynchronous reconnect that could make
	// repeated Stopped requests observe a different availability/auth state.
	client.mu.Lock()
	client.onAuthError = nil
	client.mu.Unlock()
	if mode.Transport != nil {
		client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return nil, mode.Transport
		})
	}
	if mode.Status != 0 {
		f.Upstreams[server].mu.Lock()
		f.Upstreams[server].status = mode.Status
		f.Upstreams[server].mu.Unlock()
	}
	switch mode.Name {
	case "offline":
		client.setOffline("phase7l offline")
	case "missing":
		phase7FRemoveUpstreamClient(f.App, f.Servers[server])
	case "auth preparation":
		phase7HClearUpstreamUserIdentity(client)
	case "URL preparation":
		phase7HCorruptUpstreamBaseURL(client)
	}
	return attempts
}

func phase7LAssertResponse(t *testing.T, rr *httptest.ResponseRecorder, mode phase7LMode) {
	t.Helper()
	switch mode.Name {
	case "auth preparation":
		phase7HAssertPreparationError(t, rr)
	case "URL preparation":
		phase7HAssertClientInputPreparationError(t, rr)
	default:
		phase7KAssertEmptySuccess(t, rr)
	}
}

func phase7LAssertCalls(t *testing.T, f *phase7KFixture, server int, mode phase7LMode, attempts *atomic.Int32, session string, position int64) {
	t.Helper()
	requests := f.Upstreams[server].Requests()
	wantHits := 0
	if mode.Status != 0 {
		wantHits = 1
	}
	if len(requests) != wantHits || len(f.Upstreams[1-server].Requests()) != 0 {
		t.Fatalf("unexpected upstream calls target=%d other=%d want=%d", len(requests), len(f.Upstreams[1-server].Requests()), wantHits)
	}
	wantAttempts := int32(0)
	if mode.Transport != nil {
		wantAttempts = 1
	}
	if attempts.Load() != wantAttempts {
		t.Fatalf("transport attempts=%d want=%d", attempts.Load(), wantAttempts)
	}
	if wantHits == 0 {
		return
	}
	sent := requests[0]
	gotPosition, _ := numericInt64(sent.Body["PositionTicks"])
	gotSession, _ := sent.Body["PlaySessionId"].(string)
	if sent.Path != phase7LStoppedPath || sent.Body["ItemId"] != phase7KItemID || sent.Body["UserId"] != "upstream-user-"+f.Servers[server] || gotSession != session || gotPosition != position {
		t.Fatalf("wrong forwarded Stopped: %+v", sent)
	}
}
