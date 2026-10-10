package backend

import (
	"reflect"
	"testing"
)

func phase3BWork(t *testing.T, store *IDStore, kind, server, id, name string, year any, provider string) string {
	t.Helper()
	item := map[string]any{"Id": id, "Type": kind, "Name": name, "ProductionYear": year}
	if provider != "" {
		item["ProviderIds"] = map[string]any{"Tmdb": provider}
	}
	if kind == "Movie" {
		item["MediaSources"] = []any{map[string]any{"Id": "one"}, map[string]any{"Id": "two"}}
	}
	virtual, err := store.RegisterMergeItem(newMergeCandidate(server, item, nil, false), "")
	if err != nil {
		t.Fatal(err)
	}
	return virtual
}

func phase3BLookup(t *testing.T, store *IDStore, kind, name string, year any, provider string) ([]workIdentityCandidate, bool) {
	t.Helper()
	item := map[string]any{"Type": kind, "Name": name, "ProductionYear": year}
	if provider != "" {
		item["ProviderIds"] = map[string]any{"Tmdb": provider}
	}
	result, ambiguous, err := store.FindWorkIdentityCandidates(mergeIdentityFromItem(item))
	if err != nil {
		t.Fatal(err)
	}
	return result, ambiguous
}

func TestPhase3BWorkIndexAdmissionBackfillAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A raw/legacy locator is not an independent typed work observation.
	s.GetOrCreateVirtualID("raw", "A")
	phase3BWork(t, s, "Movie", "A", "valid", "Example", 2020, "123")
	phase3BWork(t, s, "Series", "B", "show", "Series", 2022, "456")
	phase3BWork(t, s, "Episode", "C", "episode", "Example", 2020, "123")
	phase3BWork(t, s, "Season", "D", "season", "Series", 2022, "456")
	phase3BWork(t, s, "Movie", "E", "empty", "", nil, "")
	phase3BWork(t, s, "Movie", "F", "invalid-year", "Example", 0, "")
	phase3BWork(t, s, "Movie", "G", "placeholder-provider", "", nil, "unknown")
	matches, ambiguous := phase3BLookup(t, s, "Movie", "Example", 2020, "123")
	if ambiguous || len(matches) != 1 || matches[0].ServerID != "A" || matches[0].ItemID != "valid" {
		t.Fatalf("work admission: %+v, ambiguous %v", matches, ambiguous)
	}
	if n := task6SQLCount(t, s.db, "work_identity_items"); n != 2 {
		t.Fatalf("expected only 2 work rows, got %d", n)
	}
	if len(matches[0].CanonicalGroupIDs) != 1 {
		t.Fatal("lost authoritative canonical locator", matches)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	matches, ambiguous = phase3BLookup(t, reopened, "Movie", "Example", 2020, "123")
	if ambiguous || len(matches) != 1 || matches[0].ItemID != "valid" {
		t.Fatal("restart backfill failed", matches, ambiguous)
	}
	if n := task6SQLCount(t, reopened.db, "work_identity_items"); n != 2 {
		t.Fatal("backfill duplicate/omission", n)
	}
	// Retry a migration/rebuild and verify stable results.
	if err := reopened.RebuildWorkIdentityIndex(); err != nil {
		t.Fatal(err)
	}
	if n := task6SQLCount(t, reopened.db, "work_identity_items"); n != 2 {
		t.Fatal("non-idempotent rebuild", n)
	}
}

func TestPhase3BWorkIndexMultipleCandidatesNotAnAutoMerge(t *testing.T) {
	s := task6MergeStore(t)
	a := phase3BWork(t, s, "Movie", "A", "a", "Shared", 2020, "111")
	b := phase3BWork(t, s, "Movie", "B", "b", "Shared", 2020, "222")
	matches, ambiguous := phase3BLookup(t, s, "Movie", "Shared", 2020, "")
	if len(matches) != 2 || !ambiguous {
		t.Fatalf("ambiguous key incorrectly selected: %+v %v", matches, ambiguous)
	}
	if s.CanonicalMergeID(a) == s.CanonicalMergeID(b) {
		t.Fatal("lookup mutated authoritative groups")
	}
	matches, ambiguous = phase3BLookup(t, s, "Movie", "Ignored", nil, "111")
	if len(matches) != 1 || ambiguous || matches[0].ItemID != "a" {
		t.Fatal("provider candidate lookup", matches, ambiguous)
	}
}

func TestPhase3BWorkIndexAliasAbsorbAndRemoval(t *testing.T) {
	s := task6MergeStore(t)
	a := task6MergeMovie("A", "a", 100)
	b := task6MergeMovie("B", "b", 100)
	first, err := s.RegisterMergeItem(a, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterMergeItem(b, "")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := s.AssociateMergePair(a, b, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.CanonicalMergeID(first) != merged || s.CanonicalMergeID(second) != merged {
		t.Fatal("alias absorption failed")
	}
	result, ambiguous := phase3BLookup(t, s, "Movie", "Example", 2020, "100")
	if ambiguous || len(result) != 2 {
		t.Fatal("same canonical should not be ambiguous", result, ambiguous)
	}
	for _, c := range result {
		if !reflect.DeepEqual(c.CanonicalGroupIDs, []string{merged}) {
			t.Fatal("stale owner", c)
		}
	}
	if err := s.RemoveByServerID("A"); err != nil {
		t.Fatal(err)
	}
	result, ambiguous = phase3BLookup(t, s, "Movie", "Example", 2020, "100")
	if ambiguous || len(result) != 1 || result[0].ServerID != "B" {
		t.Fatal("old source index survived delete", result, ambiguous)
	}
	if err := s.RebuildWorkIdentityIndex(); err != nil {
		t.Fatal(err)
	}
	result, _ = phase3BLookup(t, s, "Movie", "Example", 2020, "100")
	if len(result) != 1 {
		t.Fatal("source resurrected by rebuild", result)
	}
}

func TestPhase3BWorkIndexIdentityChangeRemovesStaleKeys(t *testing.T) {
	s := task6MergeStore(t)
	id := phase3BWork(t, s, "Movie", "A", "a", "Old", 2020, "111")
	s.mu.Lock()
	group := s.legacyMergeGroupLocked(id, "Movie")
	for i := range group.Members {
		if group.Members[i].Ref.MediaSourceID != "" {
			continue
		}
		group.Members[i].Identity.Name = "new"
		group.Members[i].Identity.Year = 2023
		group.Members[i].Identity.YearValid = true
		group.Members[i].Identity.ProviderIDs = map[string]string{"Tmdb": "222"}
	}
	err := s.db.withWriteTx(func() error { return s.writeMergeGroupSQL(group, nil) })
	if err == nil {
		s.publishMergeGroupLocked(group, nil)
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	old, _ := phase3BLookup(t, s, "Movie", "Old", 2020, "111")
	if len(old) != 0 {
		t.Fatal("stale lookup keys survived", old)
	}
	now, _ := phase3BLookup(t, s, "Movie", "New", 2023, "222")
	if len(now) != 1 || now[0].ItemID != "a" {
		t.Fatal("new identity missing", now)
	}
}

func TestPhase3BWorkIndexAtomicRollback(t *testing.T) {
	s := task6MergeStore(t)
	if err := s.db.writeParams(`CREATE TRIGGER work_index_failure BEFORE INSERT ON work_identity_keys BEGIN SELECT RAISE(FAIL,'reject index'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := s.RegisterMergeItem(task6MergeMovie("A", "new", 100), "")
	if err == nil {
		t.Fatal("expected atomic transaction failure")
	}
	if n := task6SQLCount(t, s.db, "media_merge_groups"); n != 0 {
		t.Fatal("merge committed despite index failure", n)
	}
	if n := task6SQLCount(t, s.db, "work_identity_items"); n != 0 {
		t.Fatal("index leaked on failed transaction", n)
	}
	if err := s.db.writeParams("DROP TRIGGER work_index_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterMergeItem(task6MergeMovie("A", "new", 100), ""); err != nil {
		t.Fatal(err)
	}
	if n := task6SQLCount(t, s.db, "work_identity_items"); n != 1 {
		t.Fatal("recovery after failed transaction", n)
	}
}

func TestPhase3BWorkIndexUpgradeExistingStore(t *testing.T) {
	// Initialization on an old DB should create and populate the new derived
	// tables without inventing mappings or modifying persisted watch state.
	dir := t.TempDir()
	s, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := phase3BWork(t, s, "Movie", "A", "a", "Known", 2020, "333")
	before := s.ResolveMergeGroup(id)
	if err := s.db.writeParams("DROP TABLE work_identity_keys"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.writeParams("DROP TABLE work_identity_items"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !reflect.DeepEqual(before, s.ResolveMergeGroup(id)) {
		t.Fatal("migration touched authoritative merge")
	}
	matches, _ := phase3BLookup(t, s, "Movie", "Known", 2020, "333")
	if len(matches) != 1 {
		t.Fatal("legacy migration failed", matches)
	}
}