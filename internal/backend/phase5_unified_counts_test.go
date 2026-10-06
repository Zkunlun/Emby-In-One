package backend

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Real App stores/authentication/admin handlers remain active. The background
// scheduler is paused so publication intent can be observed without a worker
// consuming it; these tests do not claim worker throughput or startup recovery.
func pauseUnifiedCountsScheduler(t *testing.T, f *unifiedWatchFixture) *mediaCountsService {
	t.Helper()
	f.app.mediaCounts.close()
	service, err := newMediaCountsService(f.app, countsScheduleClock{now: func() time.Time { return time.Unix(1700000000, 0) }}, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		t.Error("paused scheduler invoked collector")
		return countsRoundResult{Class: countsTransport}
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.app.watchLifecycleMu.Lock()
	f.app.mediaCounts = service
	f.app.syncCountsSourcesLocked(f.app.ConfigStore.Snapshot(), false)
	service.cache.mu.Lock()
	for _, state := range service.cache.states {
		state.pending = false
		state.hasSnapshot = true
		state.snapshot = countsSnapshot{Value: mediaCounts{1, 2, 3}, DataGeneration: state.dataGeneration}
	}
	service.cache.mu.Unlock()
	f.app.watchLifecycleMu.Unlock()
	t.Cleanup(service.close)
	return service
}
func clearUnifiedCountsIntent(service *mediaCountsService) {
	service.cache.mu.Lock()
	defer service.cache.mu.Unlock()
	for _, state := range service.cache.states {
		state.pending = false
	}
}
func assertUnifiedCountsIntent(t *testing.T, service *mediaCountsService, a, b bool) {
	t.Helper()
	if service.cache.read("server-a").Pending != a || service.cache.read("server-b").Pending != b {
		t.Fatalf("counts pending A=%v B=%v", service.cache.read("server-a").Pending, service.cache.read("server-b").Pending)
	}
}
func TestUnifiedCountsAdminPublicationIntentAndRollback(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		service := pauseUnifiedCountsScheduler(t, f)
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		path := "/admin/api/users/" + f.info.UserID
		rr := doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"enabled": true, "allowedServers": []string{"server-b", "server-a", "server-a"}}, admin)
		if rr.Code != 200 {
			t.Fatalf("unchanged grant=%d %s", rr.Code, rr.Body.String())
		}
		assertUnifiedCountsIntent(t, service, false, false)
		db := f.app.IDStore.db
		if err := db.writeParams("CREATE TRIGGER unified_counts_update_fault BEFORE DELETE ON user_servers BEGIN SELECT RAISE(ABORT,'isolated grant transaction fault'); END"); err != nil {
			t.Fatal(err)
		}
		rr = doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"allowedServers": []string{"server-b"}}, admin)
		if rr.Code != 500 || f.app.lifecyclePending || !f.app.Auth.HasIssuedToken(f.token) || f.app.UserStore.Get(f.info.UserID).AuthRevision != f.info.AuthRevision {
			t.Fatalf("rollback=%d pending=%v %s", rr.Code, f.app.lifecyclePending, rr.Body.String())
		}
		if !countsSameBindings(f.app.UserStore.Get(f.info.UserID).AllowedServers, []string{"server-a", "server-b"}) {
			t.Fatal("failed transaction published grants")
		}
		assertUnifiedCountsIntent(t, service, false, false)
		if err := db.writeParams("DROP TRIGGER unified_counts_update_fault"); err != nil {
			t.Fatal(err)
		}
		rr = doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"allowedServers": []string{"server-b"}}, admin)
		if rr.Code != 200 {
			t.Fatalf("changed grant=%d %s", rr.Code, rr.Body.String())
		}
		assertUnifiedCountsIntent(t, service, false, true)
		if !service.cache.read("server-a").HasSnapshot || !service.cache.read("server-b").HasSnapshot {
			t.Fatal("user binding destroyed source success")
		}
		clearUnifiedCountsIntent(service)
		rr = doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"allowedServers": []string{"server-b"}}, admin)
		if rr.Code != 200 {
			t.Fatalf("second unchanged grant=%d", rr.Code)
		}
		assertUnifiedCountsIntent(t, service, false, false)
	})
}
func TestUnifiedCountsEmptyBindingAndUserDeletionKeepSharedCache(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		service := pauseUnifiedCountsScheduler(t, f)
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		path := "/admin/api/users/" + f.info.UserID
		rr := doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"allowedServers": []string{}}, admin)
		if rr.Code != 200 {
			t.Fatalf("empty binding=%d %s", rr.Code, rr.Body.String())
		}
		assertUnifiedCountsIntent(t, service, false, false)
		rr = doAuthJSON(t, f.handler, http.MethodDelete, path, nil, admin)
		if rr.Code != 200 {
			t.Fatalf("user delete=%d %s", rr.Code, rr.Body.String())
		}
		if !service.cache.read("server-a").HasSnapshot || !service.cache.read("server-b").HasSnapshot {
			t.Fatal("user deletion destroyed shared source success")
		}
		assertUnifiedCountsIntent(t, service, false, false)
		if got := countsTestRequest(f.handler, http.MethodGet, "/Items/Counts", admin); got.Code != 200 {
			t.Fatalf("admin cache read=%d %s", got.Code, got.Body.String())
		}
	})
}
func TestUnifiedCountsPendingCleanupAndRecoveryPublishOnce(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		service := pauseUnifiedCountsScheduler(t, f)
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		oldPath := f.app.Auth.tokenFile
		f.app.Auth.saveMu.Lock()
		f.app.Auth.tokenFile = t.TempDir()
		f.app.Auth.saveMu.Unlock()
		t.Cleanup(func() { f.app.Auth.saveMu.Lock(); f.app.Auth.tokenFile = oldPath; f.app.Auth.saveMu.Unlock() })
		path := "/admin/api/users/" + f.info.UserID
		rr := doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"allowedServers": []string{"server-b"}}, admin)
		if rr.Code != 500 || !f.app.lifecyclePending {
			t.Fatalf("pending cleanup=%d %s", rr.Code, rr.Body.String())
		}
		assertUnifiedCountsIntent(t, service, false, false)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			got := countsTestRequest(f.handler, method, "/Items/Counts", admin)
			if got.Code != http.StatusServiceUnavailable {
				t.Fatalf("pending %s Counts=%d %s", method, got.Code, got.Body.String())
			}
		}
		f.app.Auth.saveMu.Lock()
		f.app.Auth.tokenFile = oldPath
		f.app.Auth.saveMu.Unlock()
		rr = doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{}, admin)
		if rr.Code != 200 || f.app.lifecyclePending {
			t.Fatalf("cleanup recovery=%d %s", rr.Code, rr.Body.String())
		}
		assertUnifiedCountsIntent(t, service, false, true)
		if f.app.UserStore.Get(f.info.UserID).AuthRevision != f.info.AuthRevision+1 {
			t.Fatal("recovery repeated durable revision")
		}
		clearUnifiedCountsIntent(service)
		rr = doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{}, admin)
		if rr.Code != 200 {
			t.Fatalf("second recovery=%d", rr.Code)
		}
		assertUnifiedCountsIntent(t, service, false, false)
	})
}
