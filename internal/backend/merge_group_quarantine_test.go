package backend

import "testing"

func TestPhase3DWeakIdentityDriftRequiresReview(t *testing.T) {
	s := task6MergeStore(t)
	a := phase3DIdentity("A", "a", "Shared Work", "111")
	b := phase3DIdentity("B", "b", "Shared Work", "")
	if _, err := s.RegisterMergeItem(a, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterMergeItem(b, ""); err != nil {
		t.Fatal(err)
	}
	id, err := s.AssociateMergePair(a, b, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	changed := phase3DIdentity("B", "b", "Different Work", "")
	if _, err = s.RegisterMergeItem(changed, ""); err != nil {
		t.Fatal(err)
	}
	if actual := s.MergeGroupTrust(id); actual != mergeTrustReview {
		t.Fatalf("weak association remained trusted after identity drift: %s", actual)
	}
	g := s.ResolveMergeGroup(id)
	if len(g.ConflictEdges) != 0 {
		t.Fatal("weak drift incorrectly classified as provider contradiction")
	}
}

func TestPhase3DQuarantinePersistsAfterRestart(t *testing.T) {
	directory := t.TempDir()
	store, err := NewIDStore(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := phase3DIdentity("A", "a", "Shared Work", "111")
	b := phase3DIdentity("B", "b", "Shared Work", "")
	c := phase3DIdentity("C", "c", "Shared Work", "222")
	for _, v := range []mergeCandidate{a, b, c} {
		if _, err := store.RegisterMergeItem(v, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AssociateMergePair(a, b, "", "", ""); err != nil {
		t.Fatal(err)
	}
	id, err := store.AssociateMergePair(b, c, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	before := store.ResolveMergeGroup(id)
	if len(before.ConflictEdges) == 0 {
		t.Fatal("conflict not persisted before restart")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewIDStore(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if actual := reopened.MergeGroupTrust(id); actual != mergeTrustQuarantined {
		t.Fatalf("quarantine state not durable: %s", actual)
	}
	after := reopened.ResolveMergeGroup(id)
	if len(after.ConflictEdges) == 0 || len(after.Members) != len(before.Members) ||
		len(after.Aliases) != len(before.Aliases) {
		t.Fatalf("evidence or canonical state lost after restart: before=%+v after=%+v", before, after)
	}
}
