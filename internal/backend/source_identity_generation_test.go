package backend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase3EHTTPContext(t *testing.T, app *App, token string) *RequestContext {
	t.Helper()
	var captured *RequestContext
	handler := app.withContext(func(_ http.ResponseWriter, r *http.Request) {
		captured = requestContextFrom(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/Items", nil)
	req.Header.Set("X-Emby-Token", token)
	handler(httptest.NewRecorder(), req)
	if captured == nil || !captured.SourceGenerationCaptured {
		t.Fatal("HTTP request did not capture generation")
	}
	return captured
}

func phase3ESourceVersion(s *IDStore, source string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sourceGenerationLocked(source)
}

func TestPhase3EDeleteRecreateOldRequestCannotResurrect(t *testing.T) {
	item := task6HTTPMovie("reused", task6HTTPSource("version-1", 123))
	withTask6HTTPFixture(t, nil, []map[string]any{item}, func(f *task6HTTPFixture) {
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		old := phase3EHTTPContext(t, f.app, admin)
		c := newMergeCandidate("server-b", item, nil, true)
		first, err := f.app.mergeDiscovery().register(old, c, "", false)
		if err != nil || first == "" {
			t.Fatalf("initial register: %q %v", first, err)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_items"); n != 1 {
			t.Fatal("initial work index", n)
		}
		source := f.app.ConfigStore.Snapshot().Upstream[1]
		if source.ID != "server-b" {
			t.Fatalf("unexpected test source: %s", source.ID)
		}
		if v := phase3ESourceVersion(f.app.IDStore, source.ID); v != 1 {
			t.Fatal("initial epoch", v)
		}
		response := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-b", nil, admin)
		if response.Code != 200 {
			t.Fatalf("delete status %d %s", response.Code, response.Body.String())
		}
		if v := phase3ESourceVersion(f.app.IDStore, source.ID); v != 2 {
			t.Fatal("delete did not advance epoch", v)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_items"); n != 0 {
			t.Fatal("deleted source work row remains", n)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_keys"); n != 0 {
			t.Fatal("deleted source lookup key remains", n)
		}
		next := f.app.ConfigStore.Snapshot()
		next.Upstream = append(next.Upstream, source)
		if err := f.app.commitConfigSettingsOnly(next); err != nil {
			t.Fatal("recreate source", err)
		}
		if v := phase3ESourceVersion(f.app.IDStore, source.ID); v != 2 {
			t.Fatal("ID reuse reset epoch", v)
		}
		if _, err := f.app.mergeDiscovery().register(old, c, "", false); !errors.Is(err, errMediaAccessDenied) {
			t.Fatalf("old request accepted after ABA: %v", err)
		}
		f.app.registerBackgroundIDs(upstreamItemsResult{
			ServerID: source.ID, RequestScope: old, Items: []map[string]any{item}, FullSources: true,
		})
		if owners := f.app.IDStore.MergeGroupsForItem(source.ID, "reused"); len(owners) > 0 {
			t.Fatal("late drain revived old source", owners)
		}
		fresh := phase3EHTTPContext(t, f.app, admin)
		if fresh.SourceGenerations[source.ID] != 2 {
			t.Fatal("new request missing advanced epoch", fresh.SourceGenerations)
		}
		if _, err := f.app.mergeDiscovery().register(fresh, c, "", false); err != nil {
			t.Fatal("new source registration refused", err)
		}
		if len(f.app.IDStore.MergeGroupsForItem(source.ID, "reused")) == 0 {
			t.Fatal("fresh source did not register")
		}
	})
}

func TestPhase3ENormalReloadPreservesEpochAndPendingRequest(t *testing.T) {
	item := task6HTTPMovie("stable", task6HTTPSource("media-stable", 123))
	withTask6HTTPFixture(t, []map[string]any{item}, nil, func(f *task6HTTPFixture) {
		old := phase3EHTTPContext(t, f.app, f.token)
		previous := phase3ESourceVersion(f.app.IDStore, "server-a")
		if err := f.app.commitConfigSettingsOnly(f.app.ConfigStore.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if got := phase3ESourceVersion(f.app.IDStore, "server-a"); got != previous {
			t.Fatal("normal reload changed source generation", got)
		}
		if _, err := f.app.mergeDiscovery().register(old, newMergeCandidate("server-a", item, nil, true), "", false); err != nil {
			t.Fatal("legitimate in-flight passive observation rejected after reload", err)
		}
	})
}

func TestPhase3EFailedCleanupRecoveryDoesNotDoubleAdvance(t *testing.T) {
	item := task6HTTPMovie("pending", task6HTTPSource("media-pending", 123))
	withTask6HTTPFixture(t, nil, []map[string]any{item}, func(f *task6HTTPFixture) {
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		stale := phase3EHTTPContext(t, f.app, admin)
		c := newMergeCandidate("server-b", item, nil, true)
		if _, err := f.app.mergeDiscovery().register(stale, c, "", false); err != nil {
			t.Fatal(err)
		}
		if err := f.app.IDStore.db.exec(`CREATE TRIGGER p3e_fail BEFORE DELETE ON media_merge_members BEGIN SELECT RAISE(ABORT,'force delete failure'); END`); err != nil {
			t.Fatal(err)
		}
		response := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-b", nil, admin)
		if response.Code != 500 || !f.app.lifecyclePending {
			t.Fatalf("expected pending cleanup: %d %s", response.Code, response.Body.String())
		}
		if epoch := phase3ESourceVersion(f.app.IDStore, "server-b"); epoch != 1 {
			t.Fatal("failed DB transaction advanced source epoch", epoch)
		}
		if _, err := f.app.mergeDiscovery().register(stale, c, "", false); !errors.Is(err, errMediaAccessDenied) {
			t.Fatalf("pending cleanup unexpectedly admitted observation: %v", err)
		}
		if err := f.app.IDStore.db.exec("DROP TRIGGER p3e_fail"); err != nil {
			t.Fatal(err)
		}
		f.app.watchLifecycleMu.Lock()
		err := f.app.recoverWatchLifecycleLocked()
		f.app.watchLifecycleMu.Unlock()
		if err != nil {
			t.Fatal("recover pending cleanup", err)
		}
		if epoch := phase3ESourceVersion(f.app.IDStore, "server-b"); epoch != 2 {
			t.Fatal("recovered delete epoch", epoch)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_items"); n != 0 {
			t.Fatal("work index survived recovered delete", n)
		}
		f.app.watchLifecycleMu.Lock()
		err = f.app.recoverWatchLifecycleLocked()
		f.app.watchLifecycleMu.Unlock()
		if err != nil {
			t.Fatal("idempotent recovery", err)
		}
		if epoch := phase3ESourceVersion(f.app.IDStore, "server-b"); epoch != 2 {
			t.Fatal("recovery replay incremented twice", epoch)
		}
	})
}

func TestPhase3ESourceGenerationRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.db.withWriteTx(func() error { return s.advanceSourceGenerationSQL("server-x") }); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if generation := phase3ESourceVersion(s, "server-x"); generation != 2 {
		t.Fatal("generation lost on restart", generation)
	}
}
