package backend

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func mergeRevisionCandidate(kind, server, id string, ticks any, full bool) mergeCandidate {
	if kind == "Episode" {
		return newMergeCandidate(server, task6Child(kind, id, 1, 1, ticks), task6Parent(server, id), full)
	}
	return newMergeCandidate(server, task6Item(kind, id, ticks), nil, full)
}

func TestTask6MergeRevisionRuntimeIndependentIdentity(t *testing.T) {
	values := []struct {
		name  string
		value any
	}{
		{"equal", int64(34267200000)},
		{"one_tick", int64(34267200001)},
		{"jackal_8_8_seconds", int64(34179200000)},
		{"large_difference", int64(1)},
		{"null", nil},
		{"zero", 0},
		{"negative", -1},
		{"fraction", 12.5},
		{"string", "unknown"},
		{"overflow", json.Number("9223372036854775808")},
		{"imprecise_float", float64(1 << 53)},
	}
	for _, kind := range []string{"Movie", "Episode"} {
		for _, sameServer := range []bool{false, true} {
			server := "B"
			if sameServer {
				server = "A"
			}
			for _, full := range []bool{false, true} {
				for _, value := range values {
					t.Run(fmt.Sprintf("%s/same_server_%t/full_%t/%s", kind, sameServer, full, value.name), func(t *testing.T) {
						left := mergeRevisionCandidate(kind, "A", "a", int64(34267200000), full)
						right := mergeRevisionCandidate(kind, server, "b", value.value, full)
						if got := compareMergeCandidates(left, right); !got.Allowed || len(got.VersionPairs) != 1 {
							t.Fatalf("runtime or server gated identity/route: %+v", got)
						}
						if !reflect.DeepEqual(left.lookupKeys(), right.lookupKeys()) {
							t.Fatal("runtime/server/source completeness changed identity lookup")
						}
					})
				}
			}
			t.Run(fmt.Sprintf("%s/same_server_%t/missing_sources", kind, sameServer), func(t *testing.T) {
				left := mergeRevisionCandidate(kind, "A", "a", nil, true)
				raw := task6Item(kind, "b", nil)
				var parent *mergeSeriesEvidence
				if kind == "Episode" {
					raw = task6Child(kind, "b", 1, 1, nil)
					parent = task6Parent(server, "b")
				}
				delete(raw, "RunTimeTicks")
				delete(raw, "MediaSources")
				right := newMergeCandidate(server, raw, parent, false)
				if got := compareMergeCandidates(left, right); !got.Allowed || len(got.VersionPairs) != 0 {
					t.Fatalf("missing routes blocked identity or created a route: %+v", got)
				}
				if !reflect.DeepEqual(left.lookupKeys(), right.lookupKeys()) {
					t.Fatal("missing sources removed identity lookup")
				}
			})
		}
	}
}

func TestTask6MergeRevisionAllQualifiedVersions(t *testing.T) {
	for _, kind := range []string{"Movie", "Episode"} {
		for _, sameServer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same_server_%t", kind, sameServer), func(t *testing.T) {
				server := "B"
				if sameServer {
					server = "A"
				}
				raw := func(id string, count int) map[string]any {
					item := task6Item(kind, id, 900)
					if kind == "Episode" {
						item = task6Child(kind, id, 1, 1, 900)
					}
					sources := make([]any, 0, count)
					for i := 0; i < count; i++ {
						source := map[string]any{"Id": fmt.Sprintf("version-%d", i)}
						if i%3 == 0 {
							source["RunTimeTicks"] = i + 100
						} else if i%3 == 1 {
							source["RunTimeTicks"] = "unknown"
						}
						sources = append(sources, source)
					}
					item["MediaSources"] = sources
					return item
				}
				a, b := raw("a", 8), raw("b", 2)
				beforeA, _ := json.Marshal(a)
				beforeB, _ := json.Marshal(b)
				var pa, pb *mergeSeriesEvidence
				if kind == "Episode" {
					pa, pb = task6Parent("A", "a"), task6Parent(server, "b")
				}
				left, right := newMergeCandidate("A", a, pa, true), newMergeCandidate(server, b, pb, true)
				got := compareMergeCandidates(left, right)
				if !got.Allowed || len(got.VersionPairs) != 16 {
					t.Fatalf("8 by 2 eligible routes lost: %+v", got)
				}
				leftRoutes, rightRoutes := map[mergeMemberKey]bool{}, map[mergeMemberKey]bool{}
				for _, pair := range got.VersionPairs {
					leftRoutes[mergeRefKey(pair.Left)] = true
					rightRoutes[mergeRefKey(pair.Right)] = true
					if pair.Left.ServerID != "A" || pair.Left.ItemID != "a" ||
						pair.Right.ServerID != server || pair.Right.ItemID != "b" {
						t.Fatal("qualified source locator was changed", pair)
					}
				}
				if len(leftRoutes) != 8 || len(rightRoutes) != 2 {
					t.Fatal("distinct item/server routes sharing raw source IDs collapsed")
				}
				afterA, _ := json.Marshal(a)
				afterB, _ := json.Marshal(b)
				if string(beforeA) != string(afterA) || string(beforeB) != string(afterB) {
					t.Fatal("source metadata was rewritten")
				}
			})
		}
	}
}

func TestTask6MergeRevisionSavedParentLookup(t *testing.T) {
	left := mergeRevisionCandidate("Episode", "A", "a", nil, true)
	right := mergeRevisionCandidate("Episode", "A", "b", -1, true)
	left.Parent.Identity.ProviderIDs, right.Parent.Identity.ProviderIDs = nil, nil
	left.Parent.Identity.YearValid, right.Parent.Identity.YearValid = false, false
	if len(left.lookupKeys()) != 0 || len(right.lookupKeys()) != 0 {
		t.Fatal("incomplete parent metadata became ordinary identity evidence")
	}
	hint := left.savedParentLookupKey("saved-series")
	if hint == "" || hint != right.savedParentLookupKey("saved-series") {
		t.Fatal("saved parent lookup still depended on duration or source")
	}
	if compareMergeCandidates(left, right).Allowed {
		t.Fatal("a lookup hint itself became saved-parent proof")
	}
	if !compareMergeCandidatesWithSavedParent(left, right, true).Allowed {
		t.Fatal("proven saved parent failed with unknown/invalid runtime")
	}
	conflict := right
	conflict.Identity.ProviderIDs = map[string]string{"Tmdb": "different"}
	if got := compareMergeCandidatesWithSavedParent(left, conflict, true); got.Allowed || got.Reason != mergeProviderConflict {
		t.Fatal("saved parent hid child ID conflict", got)
	}
	for _, tt := range []struct {
		name   string
		season any
		index  any
		want   bool
	}{
		{"same_episode", 1, 1, true},
		{"different_season", 2, 1, false},
		{"different_episode", 1, 2, false},
		{"missing_season", nil, 1, false},
		{"missing_episode", 1, nil, false},
		{"zero_episode", 1, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidate := newMergeCandidate("B", task6Child("Episode", "b", tt.season, tt.index, 100), task6Parent("B", "b"), true)
			if got := candidate.savedParentLookupKey("saved-series"); (got == hint) != tt.want {
				t.Fatal("saved parent lookup crossed season/episode boundary", got)
			}
			if got := compareMergeCandidatesWithSavedParent(left, candidate, true); got.Allowed != tt.want {
				t.Fatal("saved parent association crossed season/episode boundary", got)
			}
		})
	}
	if left.savedParentLookupKey("") != "" || left.savedParentLookupKey("other-series") == hint {
		t.Fatal("missing/different saved owner crossed identity boundary")
	}
	unbound := left
	unbound.Parent = nil
	if unbound.savedParentLookupKey("saved-series") != "" {
		t.Fatal("missing bound parent entered saved lookup")
	}
	season := newMergeCandidate("A", task6Child("Season", "a", 0, nil, nil), task6Parent("A", "a"), true)
	if season.savedParentLookupKey("saved-series") == "" {
		t.Fatal("explicit S0 disappeared")
	}
	for _, kind := range []string{"Movie", "Series"} {
		if mergeRevisionCandidate(kind, "A", "a", nil, true).savedParentLookupKey("saved-series") != "" {
			t.Fatal("nonchild entered saved parent lookup")
		}
	}
}
