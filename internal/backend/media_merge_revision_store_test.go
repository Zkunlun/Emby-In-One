package backend

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func revisionStoreCandidate(kind, server, item string, count int) mergeCandidate {
	raw := task6Item(kind, item, nil)
	var parent *mergeSeriesEvidence
	if kind == "Episode" {
		raw = task6Child(kind, item, 1, 1, nil)
		parent = task6Parent(server, item)
	}
	sources := make([]any, 0, count)
	for i := 0; i < count; i++ {
		source := map[string]any{"Id": fmt.Sprintf("version-%d", i)}
		if i%2 == 0 {
			source["RunTimeTicks"] = i + 100
		}
		sources = append(sources, source)
	}
	raw["MediaSources"] = sources
	return newMergeCandidate(server, raw, parent, true)
}

func revisionStoredRoutes(group *mergeStoredGroup) map[mergeMemberKey]bool {
	result := map[mergeMemberKey]bool{}
	for _, member := range group.Members {
		if member.Ref.MediaSourceID != "" {
			result[mergeRefKey(member.Ref)] = true
		}
	}
	return result
}

func TestTask6MergeRevisionStoreAllVersions(t *testing.T) {
	for _, kind := range []string{"Movie", "Episode"} {
		for _, serverB := range []string{"A", "B"} {
			t.Run(kind+"/"+serverB, func(t *testing.T) {
				store := task6MergeStore(t)
				a, b := revisionStoreCandidate(kind, "A", "x", 8), revisionStoreCandidate(kind, serverB, "y", 2)
				first, err := store.RegisterMergeItem(a, "")
				if err != nil {
					t.Fatal(err)
				}
				second, err := store.RegisterMergeItem(b, "")
				if err != nil {
					t.Fatal(err)
				}
				routeA, err := store.GetMergeMediaSourceID("A", "x", "version-0")
				if err != nil {
					t.Fatal(err)
				}
				routeB, err := store.GetMergeMediaSourceID(serverB, "y", "version-0")
				if err != nil || routeA == routeB {
					t.Fatal("qualified route collision", err)
				}
				id, err := store.AssociateMergePair(a, b, "", "", first)
				if err != nil || id != first {
					t.Fatal(id, err)
				}
				group := store.ResolveMergeGroup(second)
				if len(revisionStoredRoutes(group)) != 10 || len(group.Members) != 12 || len(mergeGroupInstances(group)) != 2 {
					t.Fatal(group)
				}
				for _, c := range []mergeCandidate{a, b} {
					for _, v := range c.Versions {
						if store.ResolveMergeMember(c.ServerID, c.ItemID, v.Ref.MediaSourceID) != id {
							t.Fatal("lost source", v.Ref)
						}
					}
				}
				before := store.ResolveMergeGroup(id)
				for i := 0; i < 3; i++ {
					if got, err := store.AssociateMergePair(a, b, "", "", second); err != nil || got != id {
						t.Fatal(got, err)
					}
				}
				if !reflect.DeepEqual(before, store.ResolveMergeGroup(id)) {
					t.Fatal("repeat appended metadata or proof")
				}
				b.Identity.ProviderIDs["Tmdb"] = "changed-after-association"
				if got, err := store.AssociateMergePair(a, b, "", "", id); err != nil || got != id {
					t.Fatal("saved identity split", got, err)
				}
			})
		}
	}
}

func TestTask6MergeRevisionStoreConflictRetainsVersions(t *testing.T) {
	for _, kind := range []string{"Movie", "Episode"} {
		t.Run(kind, func(t *testing.T) {
			store := task6MergeStore(t)
			a, b := revisionStoreCandidate(kind, "A", "x", 8), revisionStoreCandidate(kind, "B", "y", 2)
			b.Identity.ProviderIDs["Tmdb"] = "conflict"
			first, err := store.RegisterMergeItem(a, "")
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.RegisterMergeItem(b, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AssociateMergePair(a, b, "", "", first); err == nil {
				t.Fatal("ID conflict accepted")
			}
			if len(revisionStoredRoutes(store.ResolveMergeGroup(first))) != 8 || len(revisionStoredRoutes(store.ResolveMergeGroup(second))) != 2 || store.CanonicalMergeID(second) != second {
				t.Fatal("conflict discarded or joined versions")
			}
		})
	}
}

func TestTask6MergeRevisionStoreMissingAndPartialSources(t *testing.T) {
	for _, kind := range []string{"Movie", "Episode"} {
		t.Run(kind, func(t *testing.T) {
			store := task6MergeStore(t)
			a, b := revisionStoreCandidate(kind, "A", "x", 0), revisionStoreCandidate(kind, "B", "y", 0)
			id, err := store.AssociateMergePair(a, b, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(revisionStoredRoutes(store.ResolveMergeGroup(id))) != 0 || store.ResolveMergeMember("A", "x", "invented") != "" {
				t.Fatal("identity anchor became playable")
			}
			full := revisionStoreCandidate(kind, "A", "x", 8)
			full.SourcesComplete = false
			if err := store.BindMergePlaceholder(id, full); err != nil {
				t.Fatal(err)
			}
			partial := revisionStoreCandidate(kind, "A", "x", 1)
			partial.SourcesComplete = false
			if err := store.BindMergePlaceholder(id, partial); err != nil {
				t.Fatal(err)
			}
			if len(revisionStoredRoutes(store.ResolveMergeGroup(id))) != 8 {
				t.Fatal("partial response pruned versions")
			}
			if err := store.BindMergePlaceholder(id, revisionStoreCandidate(kind, "B", "y", 2)); err != nil {
				t.Fatal(err)
			}
			if len(revisionStoredRoutes(store.ResolveMergeGroup(id))) != 10 {
				t.Fatal("missing-source identity failed to hydrate")
			}
			stranger := revisionStoreCandidate(kind, "C", "z", 1)
			if err := store.BindMergePlaceholder(id, stranger); err == nil {
				t.Fatal("unproven item hijacked target")
			}
		})
	}
}

func revisionSeedHistoricalGroup(t *testing.T, store *IDStore, candidate mergeCandidate, source, policy, id string) *mergeStoredGroup {
	t.Helper()
	member, err := mergeMemberFromCandidate(candidate, source)
	if err != nil {
		t.Fatal(err)
	}
	group := &mergeStoredGroup{VirtualID: id, MediaType: candidate.Identity.Type, Policy: policy, Members: []mergeStoredMember{member}}
	if policy == mergePolicyExact {
		group.Proofs = []mergeStoredProof{{Left: member, Right: member, Policy: mergePolicyExact}}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.db.withWriteTx(func() error { return store.writeMergeGroupSQL(group, nil) }); err != nil {
		t.Fatal(err)
	}
	store.publishMergeGroupLocked(group, nil)
	return cloneMergeGroup(group)
}

func TestTask6MergeRevisionStoreHistoricalAbsorptionRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := revisionStoreCandidate("Movie", "A", "x", 8)
	old := revisionSeedHistoricalGroup(t, store, a, "version-0", mergePolicyExact, "old-0")
	revisionSeedHistoricalGroup(t, store, a, "version-1", mergePolicyExact, "old-1")
	revisionSeedHistoricalGroup(t, store, a, "", mergePolicyUnresolved, "old-placeholder")
	untouched := revisionSeedHistoricalGroup(t, store, revisionStoreCandidate("Movie", "C", "z", 1), "version-0", mergePolicyExact, "unvisited")
	legacy := store.GetOrCreateVirtualID("y", "B")
	b := revisionStoreCandidate("Movie", "B", "y", 2)
	if _, err := store.CaptureLegacyMergeVersions(b); err != nil {
		t.Fatal(err)
	}
	ws, watch := task6SaveWatch(t, store, "old-0", "A", 123)
	route, err := store.GetMergeMediaSourceID("A", "x", "version-0")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws, err = NewWatchStore(store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.MergeGroupsForItem("A", "x")) != 3 {
		t.Fatal("startup changed saved groups")
	}
	id, err := store.RegisterMergeItem(a, "old-1")
	if err != nil || id != "old-1" {
		t.Fatal("caller-held target lost", id, err)
	}
	if _, err := store.AssociateMergePair(a, b, "", "", id); err != nil {
		t.Fatal(err)
	}
	group := store.ResolveMergeGroup(id)
	if len(revisionStoredRoutes(group)) != 10 || len(group.LegacyItems) != 1 || len(group.LegacyVersions) != 1 {
		t.Fatal(group)
	}
	if !reflect.DeepEqual(old.Proofs[0], group.Proofs[1]) && !reflect.DeepEqual(old.Proofs[0], group.Proofs[0]) {
		t.Fatal("historical proof overwritten")
	}
	for _, alias := range []string{"old-0", "old-placeholder", legacy} {
		if store.CanonicalMergeID(alias) != id || store.ResolveVirtualID(alias) == nil {
			t.Fatal("old ID lost", alias)
		}
	}
	if !reflect.DeepEqual(watch, ws.GetProgress("user", "old-0")) {
		t.Fatal("watch row changed")
	}
	if !reflect.DeepEqual(untouched, store.ResolveMergeGroup("unvisited")) {
		t.Fatal("unvisited group changed")
	}
	routeBefore := store.ResolveVirtualID(route)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !reflect.DeepEqual(group, store.ResolveMergeGroup(legacy)) || !reflect.DeepEqual(routeBefore, store.ResolveVirtualID(route)) {
		t.Fatal("restart lost aliases, members or route")
	}
	if len(store.MergeGroupsForItem("A", "x")) != 1 || len(store.MergeGroupsForItem("B", "y")) != 1 {
		t.Fatal("alias remained a separate owner")
	}
}

func TestTask6MergeRevisionStoreConcurrentEnrichment(t *testing.T) {
	store := task6MergeStore(t)
	full := revisionStoreCandidate("Movie", "A", "x", 8)
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 32)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		part := full
		part.Versions = []mergeVersionCandidate{full.Versions[i%8]}
		part.Versions[0].Ref.SourceIndex = 0
		part.SourcesComplete = false
		workers.Add(1)
		go func(candidate mergeCandidate) {
			defer workers.Done()
			id, err := store.RegisterMergeItem(candidate, "")
			results <- result{id, err}
		}(part)
	}
	workers.Wait()
	close(results)
	first := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if first == "" {
			first = result.id
		}
		if result.id != first {
			t.Fatal("concurrent enrichment split item")
		}
	}
	if len(revisionStoredRoutes(store.ResolveMergeGroup(first))) != 8 || task6SQLCount(t, store.db, "media_merge_groups") != 1 || task6SQLCount(t, store.db, "media_merge_members") != 9 {
		t.Fatal("concurrent update lost versions")
	}
}

func TestTask6MergeRevisionStoreQualifiedSources(t *testing.T) {
	for _, bad := range []string{"missing", "duplicate", "live", "wrong_binding"} {
		t.Run(bad, func(t *testing.T) {
			store := task6MergeStore(t)
			raw := task6Item("Movie", "x", nil)
			switch bad {
			case "missing":
				raw["MediaSources"] = []any{map[string]any{"Name": "unknown"}}
			case "duplicate":
				raw["MediaSources"] = []any{map[string]any{"Id": "dup"}, map[string]any{"Id": "dup"}}
			case "live":
				raw["MediaSources"] = []any{map[string]any{"Id": "live", "IsInfiniteStream": true}}
			}
			c := newMergeCandidate("A", raw, nil, true)
			if bad == "wrong_binding" {
				foreign := task6MergeMovie("A", "stranger", 100)
				foreign.Versions[0].Ref.MediaSourceID = task6Source("x")
				if _, err := store.RegisterMergeCandidate(foreign, task6Source("x")); err != nil {
					t.Fatal(err)
				}
				c.Versions[0].Ref.ItemID = "stranger"
			}
			id, err := store.RegisterMergeItem(c, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(revisionStoredRoutes(store.ResolveMergeGroup(id))) != 0 {
				t.Fatal("ambiguous or unbound source became a route")
			}
		})
	}
}
