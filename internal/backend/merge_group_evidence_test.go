package backend

import (
	"errors"
	"net/http"
	"testing"
)

func phase3DIdentity(server, item, name, provider string) mergeCandidate {
	raw := map[string]any{"Id": item, "Type": "Movie", "Name": name, "ProductionYear": 2024,
		"MediaSources": []any{map[string]any{"Id": "stream-" + item, "RunTimeTicks": 10000}},
	}
	if provider != "" {
		raw["ProviderIds"] = map[string]any{"Tmdb": provider}
	}
	candidate := newMergeCandidate(server, raw, nil, true)
	candidate.ObservationOrigin = "passive_detail"
	return candidate
}

func TestPhase3DIndirectProviderConflictQuarantinesCanonical(t *testing.T) {
	s := task6MergeStore(t)
	a := phase3DIdentity("A", "a", "Same Title", "111")
	b := phase3DIdentity("B", "b", "Same Title", "")
	c := phase3DIdentity("C", "c", "Same Title", "222")
	ia, err := s.RegisterMergeItem(a, "")
	if err != nil {
		t.Fatal(err)
	}
	ib, err := s.RegisterMergeItem(b, "")
	if err != nil {
		t.Fatal(err)
	}
	ic, err := s.RegisterMergeItem(c, "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AssociateMergePair(a, b, "", "", "")
	if err != nil {
		t.Fatalf("A-B weak association rejected: %v", err)
	}
	if id != ia {
		t.Fatalf("unexpected canonical shift %q vs %q", id, ia)
	}
	id, err = s.AssociateMergePair(b, c, "", "", "")
	if err != nil {
		t.Fatalf("B-C weak association rejected: %v", err)
	}
	if s.MergeGroupTrust(id) != mergeTrustQuarantined {
		t.Fatalf("indirect conflict not quarantined: %s", s.MergeGroupTrust(id))
	}
	g := s.ResolveMergeGroup(id)
	if len(g.ConflictEdges) == 0 || g.ConflictEdges[0].Namespace != "Tmdb" {
		t.Fatal("provider contradiction not attributed", g.ConflictEdges)
	}
	if len(g.Members) < 6 {
		t.Fatal("group split or lost members", len(g.Members))
	}
	if s.CanonicalMergeID(ib) != id || s.CanonicalMergeID(ic) != id {
		t.Fatal("alias broken by quarantine")
	}
	if _, err = s.AssociateMergePair(a, phase3DIdentity("D", "new", "Same Title", "111"), "", "", ""); !errors.Is(err, errMergeGroupQuarantined) {
		t.Fatal("quarantine group accepted new automatic merge", err)
	}
	// Missing newer ProviderIds must not erase the original contradiction.
	aPartial := phase3DIdentity("A", "a", "Same Title", "")
	if _, err := s.RegisterMergeItem(aPartial, ""); err != nil {
		t.Fatal(err)
	}
	if s.MergeGroupTrust(id) != mergeTrustQuarantined {
		t.Fatal("missing provider healed conflict")
	}
}

func TestPhase3DUpgradeExistingMemberAndKeepStrongEvidence(t *testing.T) {
	s := task6MergeStore(t)
	partial := phase3DIdentity("A", "a", "Same Title", "")
	id, err := s.RegisterMergeItem(partial, "")
	if err != nil {
		t.Fatal(err)
	}
	detail := phase3DIdentity("A", "a", "Same Title", "111")
	detail.SourcesComplete = false
	detail.ObservationOrigin = "passive_detail"
	if _, err = s.RegisterMergeItem(detail, id); err != nil {
		t.Fatal(err)
	}
	g := s.ResolveMergeGroup(id)
	found := false
	for _, m := range g.Members {
		if m.Ref.ServerID == "A" && m.Ref.ItemID == "a" && m.Ref.MediaSourceID == "" {
			found = true
			if m.Identity.ProviderIDs["Tmdb"] != "111" || m.Evidence == nil || len(m.Evidence.Providers) != 1 {
				t.Fatalf("existing member evidence not upgraded: %+v", m)
			}
			if len(m.Evidence.MediaSources) == 0 || !m.Evidence.MediaSources[0].Complete {
				t.Fatalf("media source completeness lost: %+v", m.Evidence)
			}
		}
	}
	if !found {
		t.Fatal("missing work anchor")
	}
	latest := phase3DIdentity("A", "a", "Same Title", "222")
	if _, err = s.RegisterMergeItem(latest, id); err != nil {
		t.Fatal(err)
	}
	if s.MergeGroupTrust(id) != mergeTrustQuarantined {
		t.Fatal("same-member contrary provider silently overwritten")
	}
	g = s.ResolveMergeGroup(id)
	for _, m := range g.Members {
		if m.Ref.MediaSourceID == "" {
			if m.Identity.ProviderIDs["Tmdb"] != "111" {
				t.Fatal("historic strong evidence erased")
			}
			if len(m.Evidence.Providers) != 2 {
				t.Fatal("provider provenance not retained", m.Evidence.Providers)
			}
		}
	}
}

func TestPhase3DQuarantineRoutesRequireQualifiedSource(t *testing.T) {
	aa := task6HTTPMovie("a", task6HTTPSource("ma", 1000))
	bb := task6HTTPMovie("b", task6HTTPSource("mb", 1000))
	cc := task6HTTPMovie("c", task6HTTPSource("mc", 1000))
	aa["ProviderIds"] = map[string]any{"Tmdb": "111"}
	delete(bb, "ProviderIds")
	cc["ProviderIds"] = map[string]any{"Tmdb": "222"}
	withTask6HTTPFixture(t, []map[string]any{aa}, []map[string]any{bb, cc}, func(f *task6HTTPFixture) {
		req := phase3CContext(f)
		a, b, c := newMergeCandidate("server-a", aa, nil, true), newMergeCandidate("server-b", bb, nil, true), newMergeCandidate("server-b", cc, nil, true)
		ia, err := f.app.registerHTTPMerge(req, a, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.app.registerHTTPMerge(req, b, ""); err != nil {
			t.Fatal(err)
		}
		if _, err = f.app.registerHTTPMerge(req, c, ""); err != nil {
			t.Fatal(err)
		}
		if _, err = f.app.associateHTTPMerge(req, a, b, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err = f.app.associateHTTPMerge(req, b, c, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if f.app.IDStore.MergeGroupTrust(ia) != mergeTrustQuarantined {
			t.Fatal("expected quarantine")
		}
		if _, err = f.app.resolveAuthorizedRouteID(req, ia); !errors.Is(err, errMergeGroupQuarantined) {
			t.Fatal("canonical selected automatically", err)
		}
		source, err := f.app.IDStore.GetMergeMediaSourceID("server-a", "a", "ma")
		if err != nil {
			t.Fatal(err)
		}
		explicit, err := f.app.resolveAuthorizedExplicitSourceRouteID(req, ia, source)
		if err != nil || explicit.ServerID != "server-a" || explicit.OriginalID != "a" {
			t.Fatalf("safe explicit selection failed %+v %v", explicit, err)
		}
		other, err := f.app.IDStore.GetMergeMediaSourceID("server-b", "c", "mc")
		if err != nil {
			t.Fatal(err)
		}
		explicit, err = f.app.resolveAuthorizedExplicitSourceRouteID(req, ia, other)
		if err != nil || explicit.ServerID != "server-b" || explicit.OriginalID != "c" {
			t.Fatalf("other explicit selection failed %+v %v", explicit, err)
		}
		if _, err = f.app.resolveAuthorizedExplicitSourceRouteID(req, ia, "invented"); err == nil {
			t.Fatal("unknown explicit route succeeded")
		}
		rr := f.request(t, http.MethodGet, "/Items/"+ia, nil, f.token)
		if rr.Code != http.StatusConflict {
			t.Fatalf("ambiguous detail returned %d %s", rr.Code, rr.Body.String())
		}
		rr = f.request(t, http.MethodGet, "/Items/"+ia+"?MediaSourceId="+source, nil, f.token)
		if rr.Code == http.StatusConflict {
			t.Fatal("explicit detail blocked", rr.Body.String())
		}
		projected := f.app.projectMergeItem(aa, "server-a", ia, req.ProxyUser.UserID)
		if len(asItems(map[string]any{"Items": projected["MediaSources"]})) != 0 {
			t.Fatal("canonical output leaked automatic versions", projected)
		}
	})
}
