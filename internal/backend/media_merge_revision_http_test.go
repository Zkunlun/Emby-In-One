package backend

import (
	"fmt"
	"net/http"
	"testing"
)

func task6SeedSeparateItems(t *testing.T, f *task6HTTPFixture, a, b map[string]any) {
	t.Helper()
	for i, item := range []map[string]any{a, b} {
		server := []string{"server-a", "server-b"}[i]
		if _, err := f.app.IDStore.RegisterMergeItem(newMergeCandidate(server, item, nil, true), ""); err != nil {
			t.Fatal(err)
		}
	}
}

func revisionHTTPItem(id, kind, series string, count int) map[string]any {
	var sources []map[string]any
	for i := 0; i < count; i++ {
		var ticks any
		if i%2 == 0 {
			ticks = 1000 + i
		}
		sources = append(sources, task6HTTPSource(fmt.Sprintf("version-%d", i), ticks))
	}
	item := task6HTTPMovie(id, sources...)
	if kind == "Episode" {
		item["Type"] = "Episode"
		item["SeriesId"] = series
		item["ParentIndexNumber"] = 1
		item["IndexNumber"] = 1
	}
	return item
}

func revisionHTTPSeries(id string) map[string]any {
	return map[string]any{"Id": id, "Type": "Series", "Name": "Example Series", "ProductionYear": 2024, "ProviderIds": map[string]any{"Tmdb": "99"}}
}

func TestTask6RevisionHTTPAllVersions(t *testing.T) {
	for _, kind := range []string{"Movie", "Episode"} {
		for _, sameServer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-server-%v", kind, sameServer), func(t *testing.T) {
				a, b := revisionHTTPItem("x", kind, "series-a", 8), revisionHTTPItem("y", kind, "series-b", 2)
				left, right := []map[string]any{a}, []map[string]any{b}
				if sameServer {
					b["SeriesId"] = "series-a"
					left = append(left, b)
					right = nil
				}
				if kind == "Episode" {
					left = append(left, revisionHTTPSeries("series-a"))
					if !sameServer {
						right = append(right, revisionHTTPSeries("series-b"))
					}
				}
				withTask6HTTPFixture(t, left, right, func(f *task6HTTPFixture) {
					rows := f.list(t, kind)
					if len(rows) != 1 || len(asItems(rows[0]["MediaSources"])) != 10 {
						t.Fatalf("list versions %+v", rows)
					}
					id := rows[0]["Id"].(string)
					detail := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+id, nil, ""))
					if len(asItems(detail["MediaSources"])) != 10 {
						t.Fatalf("detail %+v", detail)
					}
					playback := task6HTTPJSON(t, f.request(t, http.MethodPost, "/Items/"+id+"/PlaybackInfo", map[string]any{}, ""))
					versions := asItems(playback["MediaSources"])
					if len(versions) != 10 {
						t.Fatalf("playback sources=%d", len(versions))
					}
					seen := map[mergeMemberKey]bool{}
					for _, source := range versions {
						virtual := source["Id"].(string)
						mapped := f.app.IDStore.ResolveVirtualID(virtual)
						if mapped == nil || source["ItemId"] != id {
							t.Fatal("unqualified source", source)
						}
						key := mergeMemberKey{mapped.ServerID, mapped.MediaItemID, mapped.OriginalID}
						if seen[key] {
							t.Fatal("duplicate route", key)
						}
						seen[key] = true
						selected := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+id+"/PlaybackInfo?MediaSourceId="+virtual, nil, ""))
						if sources := asItems(selected["MediaSources"]); len(sources) != 1 || sources[0]["Id"] != virtual {
							t.Fatal("selection changed", selected)
						}
						rr := f.request(t, http.MethodGet, "/Videos/"+id+"/stream.mp4?MediaSourceId="+virtual, nil, "")
						label := "a"
						if mapped.ServerID == "server-b" {
							label = "b"
						}
						if want := label + ":" + mapped.MediaItemID + ":" + mapped.OriginalID; rr.Code != 200 || rr.Body.String() != want {
							t.Fatalf("route %s: %d %s", want, rr.Code, rr.Body.String())
						}
					}
				})
			})
		}
	}
}

func TestTask6RevisionHTTPConflictAndEnrichment(t *testing.T) {
	a, b := revisionHTTPItem("x", "Movie", "", 8), revisionHTTPItem("y", "Movie", "", 2)
	b["ProviderIds"] = map[string]any{"Tmdb": "other"}
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		rows := f.list(t, "Movie")
		if len(rows) != 2 {
			t.Fatal(rows)
		}
		for _, row := range rows {
			detail := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+row["Id"].(string), nil, ""))
			n := len(asItems(detail["MediaSources"]))
			if n != 8 && n != 2 {
				t.Fatal("conflict discarded versions", n)
			}
		}
	})
	small := revisionHTTPItem("x", "Movie", "", 1)
	withTask6HTTPFixture(t, []map[string]any{small}, nil, func(f *task6HTTPFixture) {
		id := f.list(t, "Movie")[0]["Id"].(string)
		f.upstream[0].update(revisionHTTPItem("x", "Movie", "", 8))
		playback := task6HTTPJSON(t, f.request(t, http.MethodPost, "/Items/"+id+"/PlaybackInfo", map[string]any{}, ""))
		if len(asItems(playback["MediaSources"])) != 8 {
			t.Fatal("newly returned playback versions lost", playback)
		}
		f.upstream[0].update(small)
		task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+id, nil, ""))
		if len(revisionStoredRoutes(f.app.IDStore.ResolveMergeGroup(id))) != 8 {
			t.Fatal("partial detail removed stored versions")
		}
	})
}

func TestTask6RevisionHTTPDirectOldDetail(t *testing.T) {
	a, b := revisionHTTPItem("x", "Movie", "", 8), revisionHTTPItem("y", "Movie", "", 2)
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		ca, cb := newMergeCandidate("server-a", a, nil, true), newMergeCandidate("server-b", b, nil, true)
		revisionSeedHistoricalGroup(t, f.app.IDStore, ca, "version-0", mergePolicyExact, "old-main")
		old := revisionSeedHistoricalGroup(t, f.app.IDStore, ca, "version-1", mergePolicyExact, "old-cut")
		member, err := mergeMemberFromCandidate(cb, "version-0")
		if err != nil {
			t.Fatal(err)
		}
		old.Members = append(old.Members, member)
		f.app.IDStore.mu.Lock()
		err = f.app.IDStore.db.withWriteTx(func() error { return f.app.IDStore.writeMergeGroupSQL(old, nil) })
		if err == nil {
			f.app.IDStore.publishMergeGroupLocked(old, nil)
		}
		f.app.IDStore.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		detail := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/old-main", nil, ""))
		if detail["Id"] != "old-main" || len(asItems(detail["MediaSources"])) != 10 {
			t.Fatal("direct detail did not discover absorbed instances", detail)
		}
		if f.app.IDStore.CanonicalMergeID("old-cut") != "old-main" {
			t.Fatal("held ID changed")
		}
	})
}
