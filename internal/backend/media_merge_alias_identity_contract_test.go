package backend

import (
	"reflect"
	"testing"
)

// Work identity (provider/name/year) is independent of media-source runtime.
// Matching TMDB proves a work on the FIRST list even if a source has no ticks.
// Learning the ticks later must never remap a client-held media ID.
func TestTask6HTTPWorkIdentityBeforeRuntimeAvailable(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", nil))
	b["RunTimeTicks"] = nil
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		task6SeedSeparateItems(t, f, a, b)
		store := f.app.IDStore
		originalA := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		originalB := task6HTTPGroup(t, f, "server-b", "movie-b", "b100")
		if originalA == originalB {
			t.Fatal("fixture must create independent historical IDs before discovery")
		}
		first := f.list(t, "Movie")
		if len(first) != 1 {
			t.Fatalf("same TMDB work must merge on first observation regardless of runtime: %+v", first)
		}
		canonical := store.CanonicalMergeID(originalA)
		if canonical == "" || canonical != store.CanonicalMergeID(originalB) {
			t.Fatalf("first list did not merge by known work identity: A=%q B=%q", canonical, store.CanonicalMergeID(originalB))
		}
		if first[0]["Id"] != canonical {
			t.Fatalf("list returned stale ID %v, expected canonical %q", first[0]["Id"], canonical)
		}
		b["MediaSources"] = []any{task6HTTPSource("b100", 1000)}
		f.upstream[1].update(b)
		after := f.list(t, "Movie")
		if len(after) != 1 || after[0]["Id"] != canonical ||
			store.CanonicalMergeID(originalA) != canonical || store.CanonicalMergeID(originalB) != canonical {
			t.Fatalf("runtime-only update changed established merged work or canonical ID: %+v", after)
		}
	})
}

// Identity hydration is the actual transition: an old B ID remains available
// as an alias when work identity is later proven, without moving old watch rows.
func TestTask6HTTPIdentityHydrationAbsorbsAliasWithoutRuntime(t *testing.T) {
	t.Run("identity_only_merge_preserves_old_row", func(t *testing.T) {
		a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
		b := task6HTTPMovie("movie-b", task6HTTPSource("b100", nil))
		b["RunTimeTicks"] = nil
		b["ProviderIds"] = nil
		b["Name"] = ""
		b["ProductionYear"] = nil
		withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
			task6SeedSeparateItems(t, f, a, b)
			store := f.app.IDStore
			canonicalA := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
			originalB := task6HTTPGroup(t, f, "server-b", "movie-b", "b100")
			if canonicalA == originalB || len(f.list(t, "Movie")) != 2 ||
				store.CanonicalMergeID(originalB) != originalB {
				t.Fatal("unproven identity must remain separate before source metadata improves")
			}
			row := &WatchProgress{
				ProxyUserID: f.user, VirtualItemID: originalB, ServerID: "server-b",
				OriginalItemID: "movie-b", ItemType: "Movie",
				PositionTicks: 200, RuntimeTicks: 1000,
				IsFavorite: true, UpdatedAt: 400, LastPlayed: 300,
			}
			if err := f.app.WatchStore.RecordProgress(row); err != nil {
				t.Fatal(err)
			}
			before := *f.app.WatchStore.GetProgress(f.user, originalB)

			b["ProviderIds"] = map[string]any{"Tmdb": "1"}
			b["Name"] = "Example"
			b["ProductionYear"] = 2024
			// Both B runtimes remain absent: identity alone must be sufficient.
			f.upstream[1].update(b)
			after := f.list(t, "Movie")
			if len(after) != 1 || after[0]["Id"] != canonicalA ||
				store.CanonicalMergeID(originalB) != canonicalA ||
				store.CanonicalMergeID(canonicalA) != canonicalA {
				t.Fatalf("identity-only update did not absorb B under A: %+v", after)
			}
			if current := f.app.WatchStore.GetProgress(f.user, originalB); current == nil ||
				!reflect.DeepEqual(before, *current) {
				t.Fatalf("absorbing alias rewrote historical watch row: before=%+v after=%+v", before, current)
			}
			for _, id := range []string{canonicalA, originalB} {
				userData := task6HTTPState(t, f, id, "")
				if userData["PlaybackPositionTicks"] != float64(200) ||
					userData["IsFavorite"] != true {
					t.Fatalf("canonical/alias did not project old user watch state, ID=%s UserData=%+v", id, userData)
				}
			}
		})
	})

	t.Run("provider_conflict_never_absorbs", func(t *testing.T) {
		a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
		b := task6HTTPMovie("movie-b", task6HTTPSource("b100", nil))
		b["RunTimeTicks"] = nil
		b["ProviderIds"] = map[string]any{"Tmdb": "other-work"}
		withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
			task6SeedSeparateItems(t, f, a, b)
			store := f.app.IDStore
			aID := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
			bID := task6HTTPGroup(t, f, "server-b", "movie-b", "b100")
			after := f.list(t, "Movie")
			if len(after) != 2 || store.CanonicalMergeID(aID) != aID ||
				store.CanonicalMergeID(bID) != bID {
				t.Fatalf("conflicting TMDB IDs must never merge even if name and year agree: %+v", after)
			}
		})
	})
}
