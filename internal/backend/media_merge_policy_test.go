package backend

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func task6Item(kind, id string, ticks any) map[string]any {
	return map[string]any{
		"Id": id, "Type": kind, "Name": "Example", "ProductionYear": float64(2020),
		"ProviderIds":  map[string]any{"Tmdb": "100"},
		"RunTimeTicks": ticks,
		"MediaSources": []any{map[string]any{"Id": "source-" + id, "RunTimeTicks": ticks}},
	}
}

func task6Pair(a, b map[string]any) mergeDecision {
	return compareMergeCandidates(newMergeCandidate("A", a, nil, true), newMergeCandidate("B", b, nil, true))
}

func TestTask6MergeIdentityRules(t *testing.T) {
	tests := []struct {
		name                string
		left, right         map[string]any
		leftName, rightName string
		leftYear, rightYear any
		want                mergeReason
	}{
		{"T6-001_tmdb", map[string]any{"Tmdb": "1"}, map[string]any{"Tmdb": "1"}, "A", "B", nil, nil, mergeAllowed},
		{"T6-002_imdb", map[string]any{"Imdb": "tt1"}, map[string]any{"Imdb": "tt1"}, "", "", nil, nil, mergeAllowed},
		{"T6-003_tvdb", map[string]any{"Tvdb": "2"}, map[string]any{"Tvdb": "2"}, "", "", nil, nil, mergeAllowed},
		{"T6-004_conflict_over_match", map[string]any{"Tmdb": "1", "Imdb": "tt1"}, map[string]any{"Tmdb": "1", "Imdb": "tt2"}, "A", "A", 2020, 2020, mergeProviderConflict},
		{"T6-005_conflict_over_name", map[string]any{"Tmdb": "1"}, map[string]any{"Tmdb": "2"}, "A", "A", 2020, 2020, mergeProviderConflict},
		{"T6-006_different_providers", map[string]any{"Tmdb": "1"}, map[string]any{"Imdb": "tt1"}, "A", "a", 2020, 2020, mergeAllowed},
		{"T6-007_no_providers", nil, nil, "A", "a", 2020, float64(2020), mergeAllowed},
		{"T6-007_one_missing", nil, map[string]any{"Tmdb": "1"}, "A", "a", 2020, json.Number("2020"), mergeAllowed},
		{"T6-008_english", nil, nil, "ABC Movie", "abc movie", 2020, 2020, mergeAllowed},
		{"T6-009_year_diff", nil, nil, "A", "a", 2020, 2021, mergeIdentityMismatch},
		{"T6-010_no_name", nil, nil, "", "", 2020, 2020, mergeIdentityMissing},
		{"T6-010_blank_name", nil, nil, " ", " ", 2020, 2020, mergeIdentityMissing},
		{"T6-010_no_year", nil, nil, "A", "a", nil, nil, mergeIdentityMissing},
		{"T6-010_bad_year", nil, nil, "A", "a", float64(2020.5), 2020, mergeIdentityMissing},
		{"T6-010_zero_year", nil, nil, "A", "a", 0, 0, mergeIdentityMissing},
		{"T6-010_negative_year", nil, nil, "A", "a", -1, -1, mergeIdentityMissing},
		{"T6-010_year_range", nil, nil, "A", "a", 10000, 10000, mergeIdentityMissing},
		{"T6-011_translation", nil, nil, "中文", "Chinese", 2020, 2020, mergeIdentityMismatch},
		{"T6-011_spaces", nil, nil, "A ", "A", 2020, 2020, mergeIdentityMismatch},
		{"T6-011_punctuation", nil, nil, "A!", "A", 2020, 2020, mergeIdentityMismatch},
		{"T6-011_nonenglish_case", nil, nil, "Ä", "ä", 2020, 2020, mergeIdentityMismatch},
		{"T6-013_namespace", map[string]any{"Tmdb": "1"}, map[string]any{"Tvdb": "1"}, "A", "B", 2020, 2020, mergeIdentityMismatch},
		{"T6-014_bad_ids", map[string]any{"Tmdb": 1, "Imdb": "", "Tvdb": " "}, map[string]any{"Tmdb": true}, "A", "a", 2020, 2020, mergeAllowed},
		{"missing_id_not_conflict", map[string]any{"Tmdb": "1", "Imdb": "tt1"}, map[string]any{"Tmdb": "1"}, "", "", nil, nil, mergeAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := map[string]any{"Type": "Movie", "Name": tt.leftName, "ProductionYear": tt.leftYear, "ProviderIds": tt.left}
			b := map[string]any{"Type": "Movie", "Name": tt.rightName, "ProductionYear": tt.rightYear, "ProviderIds": tt.right}
			left, right := mergeIdentityFromItem(a), mergeIdentityFromItem(b)
			if got := compareMergeWork(left, right); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
			if got := compareMergeWork(right, left); got != tt.want {
				t.Fatalf("reverse got %s, want %s", got, tt.want)
			}
		})
	}
	t.Run("T6-012_type_isolation", func(t *testing.T) {
		for _, kinds := range [][2]string{{"Movie", "Series"}, {"Movie", "Episode"}, {"Series", "Episode"}, {"", ""}, {"Person", "Person"}} {
			a, b := task6Item(kinds[0], "a", 100), task6Item(kinds[1], "b", 100)
			if got := task6Pair(a, b); got.Allowed || got.Reason != mergeTypeMismatch {
				t.Fatalf("%v: %+v", kinds, got)
			}
		}
	})
}

func TestTask6MergeRuntimeNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value any
		state playbackTickState
		ticks int64
	}{
		{"int", 100, playbackTicksValid, 100},
		{"int64", int64(100), playbackTicksValid, 100},
		{"float_integer", float64(100), playbackTicksValid, 100},
		{"json_integer", json.Number("100"), playbackTicksValid, 100},
		{"json_max_int64", json.Number("9223372036854775807"), playbackTicksValid, math.MaxInt64},
		{"json_beyond_float_precision", json.Number("9007199254740993"), playbackTicksValid, 9007199254740993},
		{"T6-018_null", nil, playbackTicksNull, 0},
		{"T6-018_zero", 0, playbackTicksInvalid, 0},
		{"T6-018_negative", -1, playbackTicksInvalid, 0},
		{"T6-019_fraction", float64(100.1), playbackTicksInvalid, 0},
		{"T6-019_string", "100", playbackTicksInvalid, 0},
		{"T6-019_bool", true, playbackTicksInvalid, 0},
		{"T6-019_overflow", json.Number("9223372036854775808"), playbackTicksInvalid, 0},
		{"T6-019_json_fraction", json.Number("100.0"), playbackTicksInvalid, 0},
		{"T6-019_json_exponent", json.Number("1e2"), playbackTicksInvalid, 0},
		{"T6-019_nan", math.NaN(), playbackTicksInvalid, 0},
		{"T6-019_infinity", math.Inf(1), playbackTicksInvalid, 0},
		{"T6-019_unsigned", uint64(math.MaxUint64), playbackTicksInvalid, 0},
		{"T6-019_float32", float32(100), playbackTicksInvalid, 0},
		{"T6-020_boundary", float64(1 << 53), playbackTicksInvalid, 0},
		{"T6-020_rounded", float64(9007199254740993), playbackTicksInvalid, 0},
		{"float_safe_boundary", float64((1 << 53) - 1), playbackTicksValid, (1 << 53) - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergePositiveTicks(map[string]any{"RunTimeTicks": tt.value}, "RunTimeTicks")
			if got.State != tt.state || (tt.state == playbackTicksValid && got.Ticks != tt.ticks) {
				t.Fatalf("got %+v, want state %v ticks %d", got, tt.state, tt.ticks)
			}
			a, b := task6Item("Movie", "a", tt.value), task6Item("Movie", "b", tt.value)
			if decision := task6Pair(a, b); !decision.Allowed || len(decision.VersionPairs) != 1 {
				t.Fatalf("runtime metadata must not gate identity or qualified versions: %+v", decision)
			}
		})
	}
	t.Run("T6-018_missing", func(t *testing.T) {
		if got := mergePositiveTicks(nil, "RunTimeTicks"); got.State != playbackTicksMissing {
			t.Fatal(got)
		}
		a := task6Item("Movie", "a", nil)
		delete(a, "RunTimeTicks")
		delete(a["MediaSources"].([]any)[0].(map[string]any), "RunTimeTicks")
		if !task6Pair(a, task6Item("Movie", "b", 100)).Allowed {
			t.Fatal("missing runtime blocked identity")
		}
	})
	t.Run("T6-015_016_017_028_exact_ticks", func(t *testing.T) {
		for _, diff := range []int64{0, 1, 10000000, 600000000} {
			got := task6Pair(task6Item("Movie", "a", int64(1000000000)), task6Item("Movie", "b", int64(1000000000)+diff))
			if !got.Allowed {
				t.Fatalf("diff %d: %+v", diff, got)
			}
		}
	})
}

func TestTask6MergeVersionEvidence(t *testing.T) {
	t.Run("T6-021_022_partial_versions", func(t *testing.T) {
		a, b := task6Item("Movie", "a", 100), task6Item("Movie", "b", 100)
		a["MediaSources"] = []any{
			map[string]any{"Id": "a100", "RunTimeTicks": 100},
			map[string]any{"Id": "a200", "RunTimeTicks": 200},
			map[string]any{"Id": "unknown"},
		}
		b["MediaSources"] = []any{
			map[string]any{"Id": "b100", "RunTimeTicks": 100},
			map[string]any{"Id": "b300", "RunTimeTicks": 300},
			map[string]any{"Id": "unknown"},
		}
		ca, cb := newMergeCandidate("A", a, nil, true), newMergeCandidate("B", b, nil, true)
		got := compareMergeCandidates(ca, cb)
		if !got.Allowed || len(got.VersionPairs) != 9 {
			t.Fatalf("all qualified versions must be retained regardless of runtime: %+v", got)
		}
		if ca.Versions[2].Runtime.valid() || cb.Versions[2].Runtime.valid() {
			t.Fatal("item runtime leaked into a multiversion source")
		}
	})
	t.Run("T6-023_proven_single_item_fallback", func(t *testing.T) {
		a := task6Item("Movie", "a", int64(100))
		delete(a["MediaSources"].([]any)[0].(map[string]any), "RunTimeTicks")
		ca := newMergeCandidate("A", a, nil, true)
		if ca.Versions[0].RuntimeOrigin != "proven_single_item" ||
			!compareMergeCandidates(ca, newMergeCandidate("B", task6Item("Movie", "b", 100), nil, true)).Allowed {
			t.Fatal(ca)
		}
	})
	t.Run("partial_list_not_single_proof", func(t *testing.T) {
		a := task6Item("Movie", "a", 100)
		delete(a["MediaSources"].([]any)[0].(map[string]any), "RunTimeTicks")
		if newMergeCandidate("A", a, nil, false).Versions[0].Runtime.valid() {
			t.Fatal("partial single-source list borrowed item runtime")
		}
		a["MediaSourceCount"] = 2
		if newMergeCandidate("A", a, nil, true).Versions[0].Runtime.valid() {
			t.Fatal("contradictory count proved single source")
		}
		a["MediaSourceCount"] = float64(1.5)
		if newMergeCandidate("A", a, nil, true).Versions[0].Runtime.valid() {
			t.Fatal("rounded source count proved single source")
		}
	})
	t.Run("invalid_runtime_cannot_fallback", func(t *testing.T) {
		for _, value := range []any{0, -1, "100", 100.5} {
			a := task6Item("Movie", "a", 100)
			a["MediaSources"].([]any)[0].(map[string]any)["RunTimeTicks"] = value
			if newMergeCandidate("A", a, nil, true).Versions[0].Runtime.valid() {
				t.Fatalf("invalid source value %v borrowed item runtime", value)
			}
		}
	})
	t.Run("T6-024_no_sources", func(t *testing.T) {
		for _, sources := range []any{nil, []any{}, "invalid"} {
			a := task6Item("Movie", "a", 100)
			a["MediaSources"] = sources
			if got := task6Pair(a, task6Item("Movie", "b", 100)); !got.Allowed || len(got.VersionPairs) != 0 {
				t.Fatalf("identity must not require sources or manufacture a qualified version: %+v", got)
			}
		}
	})
	t.Run("ambiguous_source_ids", func(t *testing.T) {
		for _, sources := range [][]any{
			{map[string]any{"Id": "", "RunTimeTicks": 100}},
			{map[string]any{"Id": "dup", "RunTimeTicks": 100}, map[string]any{"Id": "dup", "RunTimeTicks": 100}},
			{"invalid"},
		} {
			a := task6Item("Movie", "a", 100)
			a["MediaSources"] = sources
			if got := task6Pair(a, task6Item("Movie", "b", 100)); !got.Allowed || len(got.VersionPairs) != 0 {
				t.Fatalf("ambiguous source must not become a qualified route: %+v", got)
			}
		}
	})
	t.Run("T6-025_bound_runtime_reference", func(t *testing.T) {
		a := newMergeCandidate("A", task6Item("Movie", "a", 100), nil, true)
		b := newMergeCandidate("B", task6Item("Movie", "b", 100), nil, true)
		b.Versions[0].Ref.ServerID = "A"
		if got := compareMergeCandidates(a, b); !got.Allowed || len(got.VersionPairs) != 0 {
			t.Fatalf("cross-server source reference became a qualified route: %+v", got)
		}
		b = newMergeCandidate("B", task6Item("Movie", "b", 100), nil, true)
		b.Versions[0].Ref.ItemID = "another-item"
		if got := compareMergeCandidates(a, b); !got.Allowed || len(got.VersionPairs) != 0 {
			t.Fatalf("cross-item source reference became a qualified route: %+v", got)
		}
		unknown := task6Item("Movie", "b", nil)
		unknown["UserData"] = map[string]any{"RunTimeTicks": 100}
		candidate := newMergeCandidate("B", unknown, nil, true)
		if !compareMergeCandidates(a, candidate).Allowed || candidate.Versions[0].Runtime.valid() {
			t.Fatal("identity was gated or historical userdata runtime was borrowed")
		}
	})
	t.Run("T6-026_027_format_cut_metadata", func(t *testing.T) {
		a, b := task6Item("Movie", "a", 100), task6Item("Movie", "b", 100)
		a["MediaSources"].([]any)[0].(map[string]any)["Name"] = "Theatrical 1080p"
		b["MediaSources"].([]any)[0].(map[string]any)["Name"] = "Director 4K"
		b["MediaSources"].([]any)[0].(map[string]any)["MediaStreams"] = []any{map[string]any{"Codec": "hevc", "Language": "zho"}}
		if !task6Pair(a, b).Allowed {
			t.Fatal("format or cut label added an unapproved gate")
		}
	})
	t.Run("T6-029_live_and_unsupported", func(t *testing.T) {
		for _, live := range []map[string]any{
			{"IsInfiniteStream": true}, {"LiveStreamId": "live"},
			{"IsInfiniteStream": "false"}, {"LiveStreamId": 1},
		} {
			for _, onItem := range []bool{false, true} {
				a := task6Item("Movie", "a", 100)
				target := a["MediaSources"].([]any)[0].(map[string]any)
				if onItem {
					target = a
				}
				for k, v := range live {
					target[k] = v
				}
				if got := task6Pair(a, task6Item("Movie", "b", 100)); !got.Allowed || len(got.VersionPairs) != 0 {
					t.Fatal("live source acquired an eligible route", live, got)
				}
			}
		}
		if task6Pair(task6Item("Video", "a", 100), task6Item("Video", "b", 100)).Allowed {
			t.Fatal("unsupported item matched")
		}
	})
}

func task6Child(kind, id string, season, episode, ticks any) map[string]any {
	item := task6Item(kind, id, ticks)
	item["SeriesId"] = "series-" + id
	item["ParentIndexNumber"], item["IndexNumber"] = season, episode
	if kind == "Season" {
		item["IndexNumber"] = season
	}
	return item
}

func task6Parent(server, id string) *mergeSeriesEvidence {
	return newMergeSeriesEvidence(server, task6Item("Series", "series-"+id, nil))
}

func TestTask6MergeSeriesAndNumbers(t *testing.T) {
	t.Run("T6-031_series_no_duration_gate", func(t *testing.T) {
		for _, ticks := range []any{nil, 100, -1} {
			if !task6Pair(task6Item("Series", "a", ticks), task6Item("Series", "b", 200)).Allowed {
				t.Fatal("series runtime gate")
			}
		}
	})
	t.Run("T6-032_season_parent_and_index", func(t *testing.T) {
		a, b := task6Child("Season", "a", 1, nil, nil), task6Child("Season", "b", 1, nil, 900)
		delete(a, "SeriesId")
		a["ParentId"] = "series-a"
		a["Name"], b["Name"] = "Season one", "第一季"
		got := compareMergeCandidates(newMergeCandidate("A", a, task6Parent("A", "a"), true), newMergeCandidate("B", b, task6Parent("B", "b"), true))
		if !got.Allowed || len(got.VersionPairs) != 0 {
			t.Fatal(got)
		}
	})
	tests := []struct {
		name                                 string
		seasonA, episodeA, seasonB, episodeB any
		ticksB                               any
		want                                 mergeReason
	}{
		{"T6-033_S1E1", 1, 1, 1, 1, 100, mergeAllowed},
		{"MR-001_different_runtime", 1, 1, 1, 1, 101, mergeAllowed},
		{"MR-002_unknown_runtime", 1, 1, 1, 1, nil, mergeAllowed},
		{"T6-036_missing_season", nil, 1, nil, 1, 100, mergeNumberMissing},
		{"T6-036_missing_episode", 1, nil, 1, nil, 100, mergeNumberMissing},
		{"T6-036_bad_number", 1.5, 1, 1.5, 1, 100, mergeNumberMissing},
		{"T6-036_string_number", "1", 1, "1", 1, 100, mergeNumberMissing},
		{"T6-036_negative_season", -1, 1, -1, 1, 100, mergeNumberMissing},
		{"T6-036_zero_episode", 0, 0, 0, 0, 100, mergeNumberMissing},
		{"T6-037_specials", 0, 1, 0, 1, 100, mergeAllowed},
		{"T6-038_number_rules", 2, 3, 2, 3, 100, mergeAllowed},
		{"different_season", 1, 1, 2, 1, 100, mergeNumberMismatch},
		{"different_episode", 1, 1, 1, 2, 100, mergeNumberMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := task6Child("Episode", "a", tt.seasonA, tt.episodeA, 100)
			b := task6Child("Episode", "b", tt.seasonB, tt.episodeB, tt.ticksB)
			a["Name"], b["Name"] = "aired title", "DVD title"
			got := compareMergeCandidates(newMergeCandidate("A", a, task6Parent("A", "a"), true), newMergeCandidate("B", b, task6Parent("B", "b"), true))
			if got.Reason != tt.want || got.Allowed != (tt.want == mergeAllowed) {
				t.Fatal(got, tt.want)
			}
		})
	}
	t.Run("T6-035_different_parent", func(t *testing.T) {
		pa, pb := task6Parent("A", "a"), task6Parent("B", "b")
		pb.Identity.ProviderIDs["Tmdb"] = "other"
		got := compareMergeCandidates(newMergeCandidate("A", task6Child("Episode", "a", 1, 1, 100), pa, true), newMergeCandidate("B", task6Child("Episode", "b", 1, 1, 100), pb, true))
		if got.Allowed || got.Reason != mergeProviderConflict {
			t.Fatal(got)
		}
	})
	t.Run("T6-040_child_provider_conflict", func(t *testing.T) {
		a, b := task6Child("Episode", "a", 1, 1, 100), task6Child("Episode", "b", 1, 1, 100)
		b["ProviderIds"] = map[string]any{"Tmdb": "different"}
		got := compareMergeCandidates(newMergeCandidate("A", a, task6Parent("A", "a"), true), newMergeCandidate("B", b, task6Parent("B", "b"), true))
		if got.Allowed || got.Reason != mergeProviderConflict {
			t.Fatal(got)
		}
	})
	t.Run("T6-041_parent_proof_required", func(t *testing.T) {
		a, b := task6Child("Episode", "a", 1, 1, 100), task6Child("Episode", "b", 1, 1, 100)
		a["SeriesName"], b["SeriesName"] = "Same", "Same"
		if task6Pair(a, b).Allowed {
			t.Fatal("bare SeriesName matched")
		}
		for _, parent := range []*mergeSeriesEvidence{nil, task6Parent("B", "a"), task6Parent("A", "other")} {
			if newMergeCandidate("A", a, parent, true).Parent != nil {
				t.Fatal("unbound parent accepted")
			}
		}
		if newMergeSeriesEvidence("A", task6Item("Movie", "series-a", 100)) != nil {
			t.Fatal("movie parent accepted")
		}
		pa, pb := task6Parent("A", "a"), task6Parent("B", "b")
		pa.Identity.ProviderIDs, pb.Identity.ProviderIDs = nil, nil
		pa.Identity.YearValid, pb.Identity.YearValid = false, false
		if compareMergeCandidates(newMergeCandidate("A", a, pa, true), newMergeCandidate("B", b, pb, true)).Allowed {
			t.Fatal("episode year supplied parent year")
		}
	})
}

func TestTask6MergeCandidateLookup(t *testing.T) {
	overlap := func(a, b []string) bool {
		for _, left := range a {
			for _, right := range b {
				if left == right {
					return true
				}
			}
		}
		return false
	}
	a, b := task6Item("Movie", "a", 100), task6Item("Movie", "b", 101)
	ca, cb := newMergeCandidate("A", a, nil, true), newMergeCandidate("B", b, nil, true)
	if !overlap(ca.lookupKeys(), cb.lookupKeys()) {
		t.Fatal("different runtime missed an identity candidate bucket")
	}
	b["MediaSources"] = []any{map[string]any{"Id": "b100", "RunTimeTicks": 100}, map[string]any{"Id": "b100second", "RunTimeTicks": 100}}
	cb = newMergeCandidate("B", b, nil, true)
	if !overlap(ca.lookupKeys(), cb.lookupKeys()) || len(cb.Versions) != 2 || len(cb.lookupKeys()) != 2 {
		t.Fatal("lookup should deduplicate hints, not versions")
	}
	if len(compareMergeCandidates(ca, cb).VersionPairs) != 2 {
		t.Fatal("lost a same-runtime version")
	}
	episode := func(server, id string, season, index int) mergeCandidate {
		return newMergeCandidate(server, task6Child("Episode", id, season, index, 100), task6Parent(server, id), true)
	}
	ea, eb := episode("A", "a", 1, 1), episode("B", "b", 1, 1)
	if !overlap(ea.lookupKeys(), eb.lookupKeys()) || overlap(ea.lookupKeys(), episode("B", "b", 2, 1).lookupKeys()) ||
		overlap(ea.lookupKeys(), episode("B", "b", 1, 2).lookupKeys()) || overlap(ea.lookupKeys(), ca.lookupKeys()) {
		t.Fatal("parent/number/type boundaries missing from lookup")
	}
	unproven := newMergeCandidate("A", task6Child("Episode", "a", 1, 1, 100), nil, true)
	if len(unproven.lookupKeys()) != 0 {
		t.Fatal("unproven parent entered lookup")
	}
	series := newMergeCandidate("A", task6Item("Series", "a", nil), nil, false)
	if len(series.lookupKeys()) == 0 {
		t.Fatal("series lookup required runtime")
	}
	sa := newMergeCandidate("A", task6Child("Season", "a", 0, nil, nil), task6Parent("A", "a"), false)
	sb := newMergeCandidate("B", task6Child("Season", "b", 0, nil, nil), task6Parent("B", "b"), false)
	if !overlap(sa.lookupKeys(), sb.lookupKeys()) {
		t.Fatal("explicit Season0 lost from lookup")
	}
}

func TestTask6MergeScopeAndLookup(t *testing.T) {
	t.Run("T6-047_same_server", func(t *testing.T) {
		a, b := newMergeCandidate("A", task6Item("Movie", "a", 100), nil, true), newMergeCandidate("A", task6Item("Movie", "b", 100), nil, true)
		got := compareMergeCandidates(a, b)
		if !got.Allowed || got.Reason != mergeAllowed || len(got.VersionPairs) != 1 {
			t.Fatal("same-server identity was rejected", got)
		}
	})
	t.Run("T6-048_pairwise_no_transitive_audit", func(t *testing.T) {
		a, b, c := task6Item("Series", "a", nil), task6Item("Series", "b", nil), task6Item("Series", "c", nil)
		a["ProviderIds"], b["ProviderIds"], c["ProviderIds"] = map[string]any{"Tmdb": "1"}, nil, map[string]any{"Tmdb": "2"}
		ca, cb, cc := newMergeCandidate("A", a, nil, true), newMergeCandidate("B", b, nil, true), newMergeCandidate("C", c, nil, true)
		if !compareMergeCandidates(ca, cb).Allowed || !compareMergeCandidates(cb, cc).Allowed ||
			compareMergeCandidates(ca, cc).Reason != mergeProviderConflict {
			t.Fatal("pairwise conflict/fallback semantics changed")
		}
	})
	t.Run("lookup_hints_not_proof", func(t *testing.T) {
		a, b := task6Item("Movie", "a", 100), task6Item("Movie", "b", 100)
		a["ProviderIds"], b["ProviderIds"] = map[string]any{"Tmdb": "1", "Imdb": "tt1"}, map[string]any{"Tmdb": "1", "Imdb": "tt2"}
		left, right := mergeIdentityFromItem(a), mergeIdentityFromItem(b)
		if len(left.lookupKeys()) != 3 || left.lookupKeys()[0] != right.lookupKeys()[0] || task6Pair(a, b).Allowed {
			t.Fatal("priority lookup key used as merge proof")
		}
		first := mergeIdentity{Type: "Movie", Name: "x:y", Year: 20, YearValid: true}
		second := mergeIdentity{Type: "Movie", Name: "x", Year: 20, YearValid: true}
		if first.lookupKeys()[0] == second.lookupKeys()[0] {
			t.Fatal("lookup encoding collision")
		}
	})
	t.Run("missing_source_identity", func(t *testing.T) {
		a := task6Item("Movie", "a", 100)
		delete(a, "Id")
		if task6Pair(a, task6Item("Movie", "b", 100)).Reason != mergeSourceMissing {
			t.Fatal("missing raw item accepted")
		}
	})
	t.Run("input_metadata_immutable", func(t *testing.T) {
		a := task6Child("Episode", "a", 1, 1, 100)
		before, _ := json.Marshal(a)
		parent := task6Parent("A", "a")
		candidate := newMergeCandidate("A", a, parent, true)
		newMergeCandidate("A", a, parent, true)
		after, _ := json.Marshal(a)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("metadata mutated")
		}
		parent.Identity.ProviderIDs["Tmdb"] = "changed"
		a["ProviderIds"].(map[string]any)["Tmdb"] = "changed"
		if candidate.Identity.ProviderIDs["Tmdb"] != "100" || candidate.Parent.Identity.ProviderIDs["Tmdb"] != "100" {
			t.Fatal("candidate retained mutable metadata aliases")
		}
	})
}
