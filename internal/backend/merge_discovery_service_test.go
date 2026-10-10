package backend

import (
	"errors"
	"testing"
)

func phase3CContext(f *task6HTTPFixture) *RequestContext {
	return &RequestContext{ProxyUser: f.app.Auth.ValidateToken(f.token), ProxyToken: f.token}
}

func TestPhase3CSharedServiceRejectsMissingAndRevokedGrants(t *testing.T) {
	item := task6HTTPMovie("movie-a", task6HTTPSource("source-a", 1000))
	withTask6HTTPFixture(t, []map[string]any{item}, nil, func(f *task6HTTPFixture) {
		candidate := newMergeCandidate("server-a", item, nil, true)
		if _, err := f.app.registerHTTPMerge(nil, candidate, ""); !errors.Is(err, errMergeDiscoveryGrantRequired) {
			t.Fatalf("unauthorized nil context accepted: %v", err)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "media_merge_groups"); n != 0 {
			t.Fatal("nil request mutated work identity", n)
		}
		ctx := phase3CContext(f)
		if ctx.ProxyUser == nil {
			t.Fatal("missing fixture user")
		}
		id, err := f.app.registerHTTPMerge(ctx, candidate, "")
		if err != nil || id == "" {
			t.Fatalf("authorized register failed: %q %v", id, err)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_items"); n != 1 {
			t.Fatalf("index not synchronized: %d", n)
		}
		// A late observation from a previously authorized request cannot write
		// after the user token has been revoked.
		if !f.app.Auth.RevokeToken(f.token) {
			t.Fatal("token revocation failed")
		}
		late := task6HTTPMovie("late", task6HTTPSource("source-late", 2000))
		result := upstreamItemsResult{ServerID: "server-a", Items: []map[string]any{late}, RequestScope: ctx, FullSources: true}
		f.app.registerBackgroundIDs(result)
		if groups := f.app.IDStore.MergeGroupsForItem("server-a", "late"); len(groups) != 0 {
			t.Fatal("revoked token wrote late mapping", groups)
		}
		if _, err = f.app.registerHTTPMerge(ctx, newMergeCandidate("server-a", late, nil, true), ""); !errors.Is(err, errMediaAccessDenied) {
			t.Fatalf("revoked HTTP request was accepted: %v", err)
		}
	})
}

func TestPhase3CSharedServiceDeniedCrossSourceAssociation(t *testing.T) {
	a, b := task6HTTPMovie("left", task6HTTPSource("sa", 1000)), task6HTTPMovie("right", task6HTTPSource("sb", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		scope := phase3CContext(f)
		info := *scope.ProxyUser
		info.AllowedServers = []string{"server-a"}
		scope.ProxyUser = &info
		left := newMergeCandidate("server-a", a, nil, true)
		right := newMergeCandidate("server-b", b, nil, true)
		if _, err := f.app.associateHTTPMerge(scope, left, right, "", "", ""); !errors.Is(err, errMediaAccessDenied) {
			t.Fatalf("cross-source unauthorized association accepted: %v", err)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "media_merge_groups"); n != 0 {
			t.Fatal("unauthorized association persisted", n)
		}
	})
}

func TestPhase3CLateDiscoveryUsesSharedWriterWithoutCrossMerge(t *testing.T) {
	a, b := task6HTTPMovie("left", task6HTTPSource("sa", 1000)), task6HTTPMovie("right", task6HTTPSource("sb", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		ctx := phase3CContext(f)
		result := upstreamItemsResult{ServerID: "server-a", Items: []map[string]any{a}, RequestScope: ctx, FullSources: true}
		f.app.registerBackgroundIDs(result)
		result.ServerID = "server-b"
		result.Items = []map[string]any{b}
		f.app.registerBackgroundIDs(result)
		groupsA := f.app.IDStore.MergeGroupsForItem("server-a", "left")
		groupsB := f.app.IDStore.MergeGroupsForItem("server-b", "right")
		if len(groupsA) != 1 || len(groupsB) != 1 || groupsA[0] == groupsB[0] {
			t.Fatalf("late observation silently cross-merged: %v %v", groupsA, groupsB)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_items"); n != 2 {
			t.Fatalf("late index missing: %d", n)
		}
		// Reprocessing the same late result is idempotent.
		f.app.registerBackgroundIDs(result)
		if n := task6SQLCount(t, f.app.IDStore.db, "work_identity_items"); n != 2 {
			t.Fatalf("duplicate late index: %d", n)
		}
	})
}

func TestPhase3CAliasAndRouteHintPublication(t *testing.T) {
	a, b := task6HTTPMovie("a", task6HTTPSource("ma", 1000)), task6HTTPMovie("b", task6HTTPSource("mb", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		req := phase3CContext(f)
		left, right := newMergeCandidate("server-a", a, nil, true), newMergeCandidate("server-b", b, nil, true)
		idA, err := f.app.registerHTTPMerge(req, left, "")
		if err != nil {
			t.Fatal(err)
		}
		idB, err := f.app.registerHTTPMerge(req, right, "")
		if err != nil {
			t.Fatal(err)
		}
		if idA == idB {
			t.Fatal("preassociation groups unexpectedly identical")
		}
		owner := "user:phase3c"
		f.app.playbackRoutes.RememberMediaSource(owner, "v-source", "server-b", "negotiated-session", "client-session")
		f.app.playbackRoutes.RememberMediaSourceItem(owner, "v-source", idB, "server-b")
		before, ok := f.app.playbackRoutes.MediaSource(owner, "v-source")
		if !ok || before.ItemID != idB || before.PlaySessionID != "negotiated-session" {
			t.Fatal("route setup failed", before)
		}
		merged, err := f.app.associateHTTPMerge(req, left, right, "", "", idA)
		if err != nil || merged != idA {
			t.Fatal("association failed", merged, err)
		}
		if f.app.IDStore.CanonicalMergeID(idB) != idA {
			t.Fatal("absorbed alias was lost")
		}
		after, ok := f.app.playbackRoutes.MediaSource(owner, "v-source")
		if !ok || after.ItemID != "" || after.PlaySessionID != "negotiated-session" {
			t.Fatalf("stale route or lost negotiated lease: %+v", after)
		}
	})
}

func TestPhase3CLegacyCaptureAndMappingCommitAtomically(t *testing.T) {
	store := task6MergeStore(t)
	legacy := store.GetOrCreateVirtualID("legacy", "server-a")
	item := task6HTTPMovie("legacy", task6HTTPSource("cut-1", 1000))
	candidate := newMergeCandidate("server-a", item, nil, true)
	if err := store.db.writeParams(`CREATE TRIGGER fail_work_lookup BEFORE INSERT ON work_identity_keys BEGIN SELECT RAISE(FAIL,'index denied'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := store.RegisterMergeItemWithLegacyCapture(candidate, legacy)
	if err == nil {
		t.Fatal("expected merged capture transaction failure")
	}
	group := store.ResolveMergeGroup(legacy)
	if group.Policy != mergePolicyLegacy || len(group.LegacyVersions) != 0 || len(group.Members) != 0 {
		t.Fatalf("failed transaction committed partial legacy scope: %+v", group)
	}
	if err = store.db.writeParams("DROP TRIGGER fail_work_lookup"); err != nil {
		t.Fatal(err)
	}
	id, err := store.RegisterMergeItemWithLegacyCapture(candidate, legacy)
	if err != nil || id != legacy {
		t.Fatal("valid legacy registration failed", id, err)
	}
	group = store.ResolveMergeGroup(legacy)
	if group.Policy != mergePolicyWork || len(group.LegacyVersions) != 1 || len(group.LegacyVersions[0].SourceIDs) != 1 {
		t.Fatalf("legacy scope not captured with work association: %+v", group)
	}
}
