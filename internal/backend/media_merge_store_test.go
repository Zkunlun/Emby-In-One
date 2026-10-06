package backend

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func task6MergeStore(t *testing.T) *IDStore {
	t.Helper()
	store, err := NewIDStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !store.persistent {
		t.Fatal("test requires SQLite")
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func task6MergeMovie(server, id string, ticks any) mergeCandidate {
	return newMergeCandidate(server, task6Item("Movie", id, ticks), nil, true)
}

func task6Source(id string) string { return "source-" + id }

func task6SQLCount(t *testing.T, db *sqliteDB, table string) int64 {
	t.Helper()
	stmt, err := db.prepare("SELECT COUNT(*) FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.finalize()
	row, err := stmt.step()
	if err != nil || !row {
		t.Fatal(row, err)
	}
	return stmt.columnInt64(0)
}

func task6SaveWatch(t *testing.T, store *IDStore, id, server string, position int64) (*WatchStore, *WatchProgress) {
	t.Helper()
	ws, err := NewWatchStore(store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	row := &WatchProgress{ProxyUserID: "user", VirtualItemID: id, ServerID: server, OriginalItemID: "raw",
		ItemType: "Movie", PositionTicks: position, RuntimeTicks: 1000, Played: true, IsFavorite: true, LastPlayed: 100, UpdatedAt: 200}
	if err := ws.RecordProgress(row); err != nil {
		t.Fatal(err)
	}
	return ws, ws.GetProgress("user", id)
}

func TestTask6MergeStoreExactVersions(t *testing.T) {
	store := task6MergeStore(t)
	a := task6Item("Movie", "a", 100)
	a["MediaSources"] = []any{map[string]any{"Id": "a100", "RunTimeTicks": 100}, map[string]any{"Id": "a200", "RunTimeTicks": 200}, map[string]any{"Id": "unknown"}}
	left, right := newMergeCandidate("A", a, nil, true), task6MergeMovie("B", "b", 100)
	id, err := store.AssociateMergePair(left, right, "a100", task6Source("b"), "")
	if err != nil {
		t.Fatal(err)
	}
	group := store.ResolveMergeGroup(id)
	if group.Policy != mergePolicyWork || len(group.Members) != 6 || len(group.Proofs) != 1 || group.Proofs[0].Left.Runtime.Ticks != 100 {
		t.Fatal(group)
	}
	for _, source := range []string{"a100", "a200", "unknown"} {
		if store.ResolveMergeMember("A", "a", source) != id {
			t.Fatal("version missing", source)
		}
		if got, err := store.RegisterMergeCandidate(left, source); err != nil || got != id {
			t.Fatal(got, err)
		}
	}
	if store.GetOrCreateVirtualID("a", "A") != id || len(store.MergeGroupsForItem("A", "a")) != 1 {
		t.Fatal("item has multiple identities")
	}
	snapshot := store.ResolveMergeGroup(id)
	snapshot.Members[0].Ref.ItemID = "tampered"
	if store.ResolveMergeGroup(id).Members[0].Ref.ItemID != "a" {
		t.Fatal("snapshot aliases memory")
	}
	store.AssociateAdditionalInstance(id, "bypass", "C")
	if store.ResolveMergeMember("C", "bypass", "") != "" {
		t.Fatal("unproven legacy API bypass")
	}
}

func TestTask6MergeStoreLegacyPreservation(t *testing.T) {
	store := task6MergeStore(t)
	legacy := store.GetOrCreateVirtualID("a", "A")
	store.AssociateAdditionalInstance(legacy, "b", "B")
	ws, before := task6SaveWatch(t, store, legacy, "A", 45)
	a, b := task6MergeMovie("A", "a", nil), task6MergeMovie("B", "b", 999)
	id, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), "")
	if err != nil || id != legacy {
		t.Fatal(id, err)
	}
	if got := store.ResolveMergeGroup(id); got.Policy != mergePolicyWork || len(got.LegacyItems) != 2 {
		t.Fatal(got)
	}
	conflict := task6Item("Movie", "c", 100)
	conflict["ProviderIds"] = map[string]any{"Tmdb": "different"}
	if _, err := store.AssociateMergePair(a, newMergeCandidate("C", conflict, nil, true), task6Source("a"), task6Source("c"), legacy); err == nil {
		t.Fatal("new provider conflict accepted")
	}
	for _, ticks := range []any{nil, 101} {
		if got, err := store.AssociateMergePair(a, task6MergeMovie("C", "c", ticks), task6Source("a"), task6Source("c"), legacy); err != nil || got != legacy {
			t.Fatal(got, err)
		}
	}
	if store.ResolveMergeMember("C", "c", "unobserved") != "" {
		t.Fatal("anchor manufactured unobserved playback route")
	}
	other := store.GetOrCreateVirtualID("old-d", "D")
	if got, err := store.AssociateMergePair(a, task6MergeMovie("D", "old-d", 1), task6Source("a"), task6Source("old-d"), legacy); err != nil || got != legacy {
		t.Fatal(got, err)
	}
	if store.CanonicalMergeID(other) != legacy || store.ResolveVirtualID(other) == nil {
		t.Fatal("old ID lost")
	}
	if !reflect.DeepEqual(before, ws.GetProgress("user", legacy)) {
		t.Fatal("old watch row changed")
	}
}

func TestTask6MergeStoreLegacyVersionCapture(t *testing.T) {
	store := task6MergeStore(t)
	id := store.GetOrCreateVirtualID("a", "A")
	a := task6MergeMovie("A", "a", 100)
	partial := a
	partial.SourcesComplete = false
	if captured, err := store.CaptureLegacyMergeVersions(partial); err != nil || captured {
		t.Fatal(captured, err)
	}
	if captured, err := store.CaptureLegacyMergeVersions(a); err != nil || !captured {
		t.Fatal(captured, err)
	}
	nextItem := task6Item("Movie", "a", 200)
	nextItem["MediaSources"] = []any{map[string]any{"Id": task6Source("a"), "RunTimeTicks": 200}, map[string]any{"Id": "new-cut"}}
	next := newMergeCandidate("A", nextItem, nil, true)
	if got, err := store.RegisterMergeCandidate(next, "new-cut"); err != nil || got != id {
		t.Fatal(got, err)
	}
	if store.ResolveMergeMember("A", "a", "new-cut") != id {
		t.Fatal("newly observed version omitted")
	}
	if group := store.ResolveMergeGroup(id); len(group.LegacyVersions) != 1 || len(group.LegacyVersions[0].SourceIDs) != 1 {
		t.Fatal("historical capture overwritten", group)
	}
	if got, err := store.AssociateMergePair(next, task6MergeMovie("B", "b", 1), "new-cut", task6Source("b"), id); err != nil || got != id {
		t.Fatal(got, err)
	}
}

func TestTask6MergeStoreAliasKeepsIDsAndWatchRows(t *testing.T) {
	store := task6MergeStore(t)
	a, b := task6MergeMovie("A", "a", nil), task6MergeMovie("B", "b", 100)
	first, err := store.RegisterMergeCandidate(a, task6Source("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RegisterMergeCandidate(b, task6Source("b"))
	if err != nil {
		t.Fatal(err)
	}
	ws, firstBefore := task6SaveWatch(t, store, first, "A", 10)
	_, secondBefore := task6SaveWatch(t, store, second, "B", 20)
	id, err := store.AssociateMergePair(task6MergeMovie("A", "a", 100), b, task6Source("a"), task6Source("b"), first)
	if err != nil || id != first {
		t.Fatal(id, err)
	}
	group := store.ResolveMergeGroup(second)
	if group.VirtualID != first || len(group.Aliases) != 1 || group.Aliases[0] != second ||
		group.Proofs[0].Left.Runtime.Ticks != 100 {
		t.Fatal(group)
	}
	if !store.ContainsVirtualID(second) || store.ResolveVirtualID(second) == nil {
		t.Fatal("client-held alias no longer resolves")
	}
	if !reflect.DeepEqual(store.MergeStateIDs(second), []string{first, second}) {
		t.Fatal(store.MergeStateIDs(second))
	}
	if !reflect.DeepEqual(firstBefore, ws.GetProgress("user", first)) || !reflect.DeepEqual(secondBefore, ws.GetProgress("user", second)) {
		t.Fatal("watch rows migrated/erased while establishing association")
	}
}

func TestTask6MergeStoreRestartAndSavedMetadata(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := task6MergeMovie("A", "a", 100), task6MergeMovie("B", "b", 100)
	first, err := store.RegisterMergeCandidate(a, task6Source("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RegisterMergeCandidate(b, task6Source("b"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), first)
	if err != nil {
		t.Fatal(err)
	}
	ws, before := task6SaveWatch(t, store, id, "A", 30)
	snapshot := store.ResolveMergeGroup(id)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(snapshot, reopened.ResolveMergeGroup(second)) {
		t.Fatal("restart lost members/proof/aliases")
	}
	newWS, err := NewWatchStore(reopened.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, newWS.GetProgress("user", id)) {
		t.Fatal("restart altered watch row")
	}
	_ = ws
	changedA, changedB := task6MergeMovie("A", "a", nil), task6MergeMovie("B", "b", 500)
	got, err := reopened.AssociateMergePair(changedA, changedB, task6Source("a"), task6Source("b"), "")
	if err != nil || got != id {
		t.Fatal("saved association was split on metadata edit", got, err)
	}
	if !reflect.DeepEqual(snapshot, reopened.ResolveMergeGroup(id)) {
		t.Fatal("saved proof overwritten by later invalid metadata")
	}
}

func TestTask6MergeStoreAtomicFailures(t *testing.T) {
	for _, table := range []string{"media_merge_groups", "media_merge_members", "media_merge_aliases", "id_mappings", "id_additional_instances"} {
		t.Run(table, func(t *testing.T) {
			store := task6MergeStore(t)
			a, b := task6MergeMovie("A", "a", 100), task6MergeMovie("B", "b", 100)
			first, err := store.RegisterMergeCandidate(a, task6Source("a"))
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.RegisterMergeCandidate(b, task6Source("b"))
			if err != nil {
				t.Fatal(err)
			}
			beforeA, beforeB := store.ResolveMergeGroup(first), store.ResolveMergeGroup(second)
			if err := store.db.writeParams("CREATE TRIGGER merge_fault BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT, 'isolated merge fault'); END"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), first); err == nil {
				t.Fatal("injected write failure ignored")
			}
			if !reflect.DeepEqual(beforeA, store.ResolveMergeGroup(first)) || !reflect.DeepEqual(beforeB, store.ResolveMergeGroup(second)) ||
				store.ResolveMergeMember("B", "b", task6Source("b")) != second {
				t.Fatal("failed transaction published a partial memory association")
			}
			if task6SQLCount(t, store.db, "media_merge_groups") != 2 || task6SQLCount(t, store.db, "media_merge_members") != 4 || task6SQLCount(t, store.db, "media_merge_aliases") != 0 {
				t.Fatal("failed transaction left partial database rows")
			}
			if err := store.db.writeParams("DROP TRIGGER merge_fault"); err != nil {
				t.Fatal(err)
			}
			id, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), first)
			if err != nil || id != first {
				t.Fatal("retry did not converge", id, err)
			}
			if task6SQLCount(t, store.db, "media_merge_groups") != 1 || task6SQLCount(t, store.db, "media_merge_aliases") != 1 {
				t.Fatal("retry duplicated identity")
			}
		})
	}
}

func TestTask6MergeStoreConcurrentIdempotence(t *testing.T) {
	store := task6MergeStore(t)
	a, b := task6MergeMovie("A", "a", 100), task6MergeMovie("B", "b", 100)
	type outcome struct {
		id  string
		err error
	}
	results := make(chan outcome, 32)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			id, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), "")
			results <- outcome{id, err}
		}()
	}
	workers.Wait()
	close(results)
	identity := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if identity == "" {
			identity = result.id
		}
		if result.id != identity {
			t.Fatal("concurrent duplicate discovery issued conflicting IDs")
		}
	}
	if task6SQLCount(t, store.db, "media_merge_groups") != 1 || task6SQLCount(t, store.db, "media_merge_members") != 4 {
		t.Fatal("concurrent duplicate registration persisted duplicate rows")
	}
	if len(store.ResolveMergeGroup(identity).Proofs) != 1 {
		t.Fatal("idempotent observation added duplicate proof")
	}
}

func TestTask6MergeStoreQualifiedGuards(t *testing.T) {
	t.Run("same_server_and_unconfigured", func(t *testing.T) {
		store := task6MergeStore(t)
		if _, err := store.AssociateMergePair(task6MergeMovie("A", "a", 100), task6MergeMovie("A", "b", 100), task6Source("a"), task6Source("b"), ""); err != nil {
			t.Fatal("same server identity not merged", err)
		}
		store.configuredSources = map[string]bool{"A": true}
		if _, err := store.RegisterMergeCandidate(task6MergeMovie("B", "b", 100), task6Source("b")); err == nil {
			t.Fatal("removed/unconfigured source published")
		}
	})
	t.Run("legacy_fallback_stays_source_qualified", func(t *testing.T) {
		store := task6MergeStore(t)
		legacy := store.GetOrCreateVirtualID("movie:one", "source")
		if store.ResolveMergeMember("one:source", "movie", task6Source("movie")) != "" ||
			len(store.MergeGroupsForItem("one:source", "movie")) != 0 {
			t.Fatal("legacy delimiter collision became cross-source proof")
		}
		next, err := store.RegisterMergeCandidate(task6MergeMovie("one:source", "movie", 100), task6Source("movie"))
		if err != nil || next == legacy {
			t.Fatal(next, err)
		}
		if store.ResolveMergeMember("source", "movie:one", "old-version") != legacy {
			t.Fatal("genuine legacy binding lost")
		}
	})
	t.Run("target_must_own_pair", func(t *testing.T) {
		store := task6MergeStore(t)
		unrelated, err := store.RegisterMergeCandidate(task6MergeMovie("C", "c", 100), task6Source("c"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AssociateMergePair(task6MergeMovie("A", "a", 100), task6MergeMovie("B", "b", 100), task6Source("a"), task6Source("b"), unrelated); err == nil {
			t.Fatal("arbitrary target hijacked members")
		}
	})
	t.Run("no_persistence_or_closed", func(t *testing.T) {
		memory := &IDStore{mergeState: emptyMergeStoreState()}
		if _, err := memory.RegisterMergeCandidate(task6MergeMovie("A", "a", 100), task6Source("a")); err != errMergeStoreUnavailable {
			t.Fatal(err)
		}
		store := task6MergeStore(t)
		_ = store.Close()
		if _, err := store.RegisterMergeCandidate(task6MergeMovie("A", "a", 100), task6Source("a")); err != errMergeStoreUnavailable {
			t.Fatal(err)
		}
	})
}

func TestTask6MergeStoreConfirmedParent(t *testing.T) {
	store := task6MergeStore(t)
	parent := store.GetOrCreateVirtualID("series-a", "A")
	store.AssociateAdditionalInstance(parent, "series-b", "B")
	a := task6Child("Episode", "a", 1, 1, 100)
	b := task6Child("Episode", "b", 1, 1, 100)
	pa, pb := task6Parent("A", "a"), task6Parent("B", "b")
	pb.Identity.ProviderIDs["Tmdb"] = "changed-parent-metadata"
	left, right := newMergeCandidate("A", a, pa, true), newMergeCandidate("B", b, pb, true)
	if compareMergeCandidates(left, right).Allowed {
		t.Fatal("fixture should require preserved parent proof")
	}
	id, err := store.AssociateMergePair(left, right, task6Source("a"), task6Source("b"), "")
	if err != nil || store.ResolveMergeGroup(id).Proofs[0].ParentGroupID != parent {
		t.Fatal(id, err)
	}
	store2 := task6MergeStore(t)
	if _, err := store2.AssociateMergePair(left, right, task6Source("a"), task6Source("b"), ""); err == nil {
		t.Fatal("unregistered parent conflict bypassed")
	}
}

func TestTask6MergeStoreServerRemoval(t *testing.T) {
	store := task6MergeStore(t)
	a, b := task6MergeMovie("A", "a", 100), task6MergeMovie("B", "b", 100)
	first, _ := store.RegisterMergeCandidate(a, task6Source("a"))
	second, _ := store.RegisterMergeCandidate(b, task6Source("b"))
	id, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), first)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveByServerID("A"); err != nil {
		t.Fatal(err)
	}
	if store.ResolveMergeMember("A", "a", task6Source("a")) != "" || store.ResolveMergeMember("B", "b", task6Source("b")) != id {
		t.Fatal("deleted server membership survived or surviving member lost identity")
	}
	if group := store.ResolveMergeGroup(second); group.VirtualID != id || len(group.Members) != 2 || store.ResolveVirtualID(second).ServerID != "B" {
		t.Fatal("promotion/alias scope changed", group)
	}
	if err := store.RemoveByServerID("B"); err != nil {
		t.Fatal(err)
	}
	if store.ResolveMergeGroup(id) != nil || store.ContainsVirtualID(second) || task6SQLCount(t, store.db, "media_merge_groups") != 0 {
		t.Fatal("last source left orphan identities")
	}
}

func TestTask6MergeStoreRemovalFailure(t *testing.T) {
	store := task6MergeStore(t)
	a, b := task6MergeMovie("A", "a", 100), task6MergeMovie("B", "b", 100)
	id, err := store.AssociateMergePair(a, b, task6Source("a"), task6Source("b"), "")
	if err != nil {
		t.Fatal(err)
	}
	before := store.ResolveMergeGroup(id)
	if err := store.db.writeParams("CREATE TRIGGER merge_delete_fault BEFORE DELETE ON media_merge_members BEGIN SELECT RAISE(ABORT,'isolated removal fault'); END"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveByServerID("A"); err == nil {
		t.Fatal("removal failure ignored")
	}
	if !reflect.DeepEqual(before, store.ResolveMergeGroup(id)) || store.ResolveVirtualID(id).ServerID != "A" {
		t.Fatal("failed removal changed published locator")
	}
	if err := store.db.writeParams("DROP TRIGGER merge_delete_fault"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveByServerID("A"); err != nil {
		t.Fatal(err)
	}
}

func TestTask6MergeStoreUpgradeExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := openSQLite(filepath.Join(dir, "mappings.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.exec(`
CREATE TABLE id_mappings(virtual_id TEXT PRIMARY KEY, original_id TEXT NOT NULL, server_id TEXT NOT NULL);
CREATE TABLE id_additional_instances(virtual_id TEXT NOT NULL, original_id TEXT NOT NULL, server_id TEXT NOT NULL, UNIQUE(virtual_id, original_id, server_id));
INSERT INTO id_mappings VALUES('old-client-id', 'a', 'A');
INSERT INTO id_additional_instances VALUES('old-client-id', 'b', 'B');`); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWatchStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := &WatchProgress{ProxyUserID: "user", VirtualItemID: "old-client-id", ServerID: "A", OriginalItemID: "a",
		PositionTicks: 50, RuntimeTicks: 0, Played: true, IsFavorite: true, LastPlayed: 100, UpdatedAt: 200}
	if err := ws.RecordProgress(row); err != nil {
		t.Fatal(err)
	}
	before := ws.GetProgress("user", "old-client-id")
	if err := closeSQLite(db); err != nil {
		t.Fatal(err)
	}
	upgraded, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	newWS, err := NewWatchStore(upgraded.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, newWS.GetProgress("user", "old-client-id")) ||
		upgraded.ResolveMergeMember("B", "b", "unknown-old-version") != "old-client-id" ||
		upgraded.ResolveVirtualID("old-client-id").OriginalID != "a" {
		t.Fatal("schema addition altered old ID, relationship or watch data")
	}
	if task6SQLCount(t, upgraded.db, "media_merge_groups") != 0 || task6SQLCount(t, upgraded.db, "id_mappings") != 1 ||
		task6SQLCount(t, upgraded.db, "id_additional_instances") != 1 {
		t.Fatal("upgrade rewrote old mappings")
	}
}

func TestTask6MergeStoreLifecycleTransactionRecovery(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		a := task6MergeMovie("server-a", "exact-a", 1000)
		b := task6MergeMovie("server-b", "exact-b", 1000)
		store := f.app.IDStore
		first, err := store.RegisterMergeCandidate(a, task6Source("exact-a"))
		if err != nil {
			t.Fatal(err)
		}
		alias, err := store.RegisterMergeCandidate(b, task6Source("exact-b"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := store.AssociateMergePair(a, b, task6Source("exact-a"), task6Source("exact-b"), first)
		if err != nil {
			t.Fatal(err)
		}
		row := &WatchProgress{ProxyUserID: f.info.UserID, VirtualItemID: alias, ServerID: "server-a", OriginalItemID: "exact-a",
			ItemType: "Movie", PositionTicks: 400, RuntimeTicks: 1000, Played: true, IsFavorite: true, LastPlayed: 100, UpdatedAt: 200}
		if err := f.app.WatchStore.RecordProgress(row); err != nil {
			t.Fatal(err)
		}
		before := f.app.WatchStore.GetProgress(f.info.UserID, alias)
		beforeGroup := store.ResolveMergeGroup(id)
		if err := store.db.writeParams("CREATE TRIGGER merge_lifecycle_fault BEFORE DELETE ON media_merge_members BEGIN SELECT RAISE(ABORT,'isolated merge lifecycle fault'); END"); err != nil {
			t.Fatal(err)
		}
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		response := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-a", nil, admin)
		if response.Code != 500 || !f.app.lifecyclePending {
			t.Fatal("expected pending committed deletion", response.Code)
		}
		if !reflect.DeepEqual(beforeGroup, store.ResolveMergeGroup(id)) || !reflect.DeepEqual(before, f.app.WatchStore.GetProgress(f.info.UserID, alias)) {
			t.Fatal("failed lifecycle transaction published partial version or watch changes")
		}
		if err := store.db.writeParams("DROP TRIGGER merge_lifecycle_fault"); err != nil {
			t.Fatal(err)
		}
		response = doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.info.UserID, map[string]any{}, admin)
		if response.Code != 200 || f.app.lifecyclePending {
			t.Fatal("cleanup recovery failed", response.Code)
		}
		group := store.ResolveMergeGroup(alias)
		if group == nil || group.VirtualID != id || len(group.Members) != 2 || group.Members[0].Ref.ServerID != "server-b" {
			t.Fatal("cleanup lost precise membership or alias", group)
		}
		after := f.app.WatchStore.GetProgress(f.info.UserID, alias)
		if after == nil || after.PositionTicks != before.PositionTicks || after.Played != before.Played ||
			after.IsFavorite != before.IsFavorite || after.ServerID != "server-b" || after.RuntimeTicks != 0 ||
			after.LastPlayed != before.LastPlayed || after.UpdatedAt != before.UpdatedAt {
			t.Fatal("surviving watch row not preserved", after)
		}
		if store.ResolveMergeMember("server-a", "exact-a", task6Source("exact-a")) != "" {
			t.Fatal("deleted source still owns version")
		}
		if _, err := store.RegisterMergeCandidate(a, task6Source("exact-a")); err == nil {
			t.Fatal("late registration resurrected deleted source")
		}
	})
}

func TestTask6MergeStoreCorruptSnapshotRejects(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.RegisterMergeCandidate(task6MergeMovie("A", "a", 100), task6Source("a"))
	if err != nil {
		t.Fatal(err)
	}
	group := store.ResolveMergeGroup(id)
	group.Members[0].Ref.ItemID = "tampered"
	payload, _ := json.Marshal(group)
	if err := store.db.writeParams("UPDATE media_merge_groups SET snapshot = ? WHERE virtual_id = ?", string(payload), id); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := NewIDStore(dir, nil)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("corrupt index/snapshot silently loaded as legacy")
	}
}
