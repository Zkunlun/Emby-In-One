package backend

import (
	"testing"
)

func TestRemoveByServerIDCascadeClean(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewIDStore(tempDir, nil)
	if err != nil {
		t.Fatalf("NewIDStore: %v", err)
	}
	defer store.Close()

	// 1. virtA is primarily mapped to srv-A ("movie-1", "srv-A")
	virtA := store.GetOrCreateVirtualID("movie-1", "srv-A")

	// Associate an additional instance from srv-B onto virtA
	store.AssociateAdditionalInstance(virtA, "movie-1-b", "srv-B")

	// 2. virtB is primarily mapped to srv-B ("movie-2", "srv-B")
	virtB := store.GetOrCreateVirtualID("movie-2", "srv-B")

	// Associate an additional instance from srv-A onto virtB
	store.AssociateAdditionalInstance(virtB, "movie-2-a", "srv-A")

	// 3. virtC is purely on srv-C
	virtC := store.GetOrCreateVirtualID("movie-3", "srv-C")

	// Verify all exist before deletion
	if r := store.ResolveVirtualID(virtA); r == nil || len(r.OtherInstances) != 1 {
		t.Fatalf("virtA setup incorrect: %+v", r)
	}
	if r := store.ResolveVirtualID(virtB); r == nil || len(r.OtherInstances) != 1 {
		t.Fatalf("virtB setup incorrect: %+v", r)
	}

	// Delete srv-A. virtA has a surviving srv-B instance, so its virtual identity
	// must be preserved and srv-B promoted to primary.
	result, err := store.RemoveByServerIDPreservingInstances("srv-A")
	if err != nil {
		t.Fatalf("RemoveByServerIDPreservingInstances(srv-A): %v", err)
	}
	if len(result.PromotedVirtualIDs) != 1 || result.PromotedVirtualIDs[0] != virtA {
		t.Fatalf("promoted = %#v, want [%s]", result.PromotedVirtualIDs, virtA)
	}
	if len(result.RemovedVirtualIDs) != 0 {
		t.Fatalf("removed = %#v, want none", result.RemovedVirtualIDs)
	}

	// Verify:
	// a. virtA survives under the same virtual ID, now routed to srv-B.
	rA := store.ResolveVirtualID(virtA)
	if rA == nil || rA.ServerID != "srv-B" || rA.OriginalID != "movie-1-b" {
		t.Fatalf("virtA was not promoted correctly: %+v", rA)
	}
	if r := store.ResolveByOriginalID("movie-1"); r != nil {
		t.Errorf("deleted srv-A original should not be resolvable, got: %+v", r)
	}
	if r := store.ResolveByOriginalID("movie-1-b"); r == nil || r.ServerID != "srv-B" {
		t.Errorf("promoted srv-B original should resolve, got: %+v", r)
	}

	// b. virtB must still exist on srv-B, but its additional instance from srv-A must be gone!
	rB := store.ResolveVirtualID(virtB)
	if rB == nil {
		t.Fatalf("virtB should still exist on srv-B")
	}
	if rB.ServerID != "srv-B" || rB.OriginalID != "movie-2" {
		t.Errorf("virtB mismatch: %+v", rB)
	}
	if len(rB.OtherInstances) != 0 {
		t.Errorf("virtB other instances should be empty after srv-A removed, but got: %+v", rB.OtherInstances)
	}

	// c. virtC must be untouched
	rC := store.ResolveVirtualID(virtC)
	if rC == nil || rC.ServerID != "srv-C" {
		t.Errorf("virtC affected unexpectedly: %+v", rC)
	}
}
