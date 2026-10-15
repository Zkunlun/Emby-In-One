package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Only the requested item's detail may discover more sources. The fixture
// models a single upstream serving a partial /Items/{id} yet complete
// PlaybackInfo, which is a compatibility scenario, not an SNTP API capture.
func TestPhase4SNTPDetailRecoversNewVersionOnlyOnDemand(t *testing.T) {
	for _, kind := range []string{"Movie", "Episode"} {
		t.Run(kind, func(t *testing.T) {
			const itemID = "phase4-item"
			first := task6HTTPSource("cut-a", 1000)
			second := task6HTTPSource("cut-b", 1000)
			second["Name"] = "Higher Quality Cut"
			second["DirectStreamUrl"] = "/Videos/phase4-item/stream.mp4?api_key=UPSTREAM_SENSITIVE"
			second["TranscodingUrl"] = "/Videos/phase4-item/master.m3u8?api_key=UPSTREAM_SENSITIVE"
			second["Path"] = "https://upstream.example/private/file?token=UPSTREAM_SENSITIVE"
			second["MediaStreams"] = []any{map[string]any{
				"Codec":       "h264",
				"DeliveryUrl": "https://upstream.example/private/captions?api_key=UPSTREAM_SENSITIVE",
			}}
			full := task6HTTPMovie(itemID, first, second)
			full["Name"] = "Fixture multi-cut"
			if kind == "Episode" {
				full["Type"] = "Episode"
				full["SeriesId"] = "phase4-series"
				full["ParentIndexNumber"] = 1
				full["IndexNumber"] = 4
			}
			partial := deepCloneMap(full)
			partial["MediaSources"] = []any{deepCloneMap(first)}
			delete(partial, "MediaSourceCount")

			var detailHits, playbackHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"AccessToken": "fixture-access-token",
						"User":        map[string]any{"Id": "phase4-user"},
					})
				case r.URL.Path == "/System/Info/Public":
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "phase4-upstream"})
				case r.Method == http.MethodGet && (r.URL.Path == "/Items/"+itemID || r.URL.Path == "/Users/phase4-user/Items/"+itemID):
					detailHits.Add(1)
					_ = json.NewEncoder(w).Encode(partial)
				case r.Method == http.MethodPost && r.URL.Path == "/Items/"+itemID+"/PlaybackInfo":
					playbackHits.Add(1)
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if r.URL.Query().Get("UserId") != "phase4-user" || body["UserId"] != "phase4-user" {
						t.Errorf("PlaybackInfo on-demand must use authenticated upstream user")
					}
					if body["MediaSourceId"] != "" && body["MediaSourceId"] != nil {
						t.Errorf("on-demand discovery must not select a single source")
					}
					if body["IsPlayback"] != false {
						t.Errorf("metadata probe must not claim playback, got IsPlayback=%v", body["IsPlayback"])
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"PlaySessionId": "must-not-leak-session",
						"MediaSources":  full["MediaSources"],
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()

			withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
				client := app.Upstream.Clients()[0]
				initial := newMergeCandidate(client.ID, deepCloneMap(partial), nil, false)
				virtual, err := app.IDStore.RegisterMergeItem(initial, "")
				if err != nil {
					t.Fatal(err)
				}
				if app.IDStore.MergeMemberAllowed(virtual, client.ID, itemID, "cut-b") {
					t.Fatal("test precondition: second version must not be known")
				}
				// List processing under an authorized request must preserve the
				// media tile without issuing a per-row PlaybackInfo probe.
				request := httptest.NewRequest(http.MethodGet, "/Items?Recursive=true", nil)
				permission := &RequestContext{
					Headers:   request.Header.Clone(),
					ProxyUser: &tokenInfo{UserID: "phase4-fixture-admin", Role: "admin"},
				}
				request = request.WithContext(context.WithValue(request.Context(), requestContextKey{}, permission))
				rows := app.rewriteMergeResponseItems(request, []map[string]any{deepCloneMap(partial)}, client.ID, false)
				if len(rows) != 1 || playbackHits.Load() != 0 {
					t.Fatalf("library list must preserve tile without PlaybackInfo probing, rows=%d calls=%d", len(rows), playbackHits.Load())
				}

				rr := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+virtual, virtual)
				if rr.Code != http.StatusOK {
					t.Fatalf("detail status=%d,body=%s", rr.Code, rr.Body.String())
				}
				var item map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
					t.Fatal(err)
				}
				sources := asItems(item["MediaSources"])
				if len(sources) != 2 || item["MediaSourceCount"] != float64(2) {
					t.Fatalf("%s detail needs 2 usable versions, got %d count %v", kind, len(sources), item["MediaSourceCount"])
				}
				if playbackHits.Load() != 1 || detailHits.Load() != 1 {
					t.Fatalf("detail should use exactly one extra probe, detail GET=%d playback POST=%d", detailHits.Load(), playbackHits.Load())
				}
				found := map[string]bool{}
				for _, source := range sources {
					vid, _ := source["Id"].(string)
					route := app.IDStore.ResolveVirtualID(vid)
					if route == nil || route.ServerID != client.ID || route.MediaItemID != itemID {
						t.Fatalf("invalid source route %q", vid)
					}
					found[route.OriginalID] = true
					if source["ItemId"] != virtual {
						t.Errorf("source ItemId not virtual: %v", source["ItemId"])
					}
				}
				if !found["cut-a"] || !found["cut-b"] || !app.IDStore.MergeMemberAllowed(virtual, client.ID, itemID, "cut-b") {
					t.Fatalf("new source was not registered and projected: sources=%v membership=%v", found, app.IDStore.MergeMemberAllowed(virtual, client.ID, itemID, "cut-b"))
				}
				// Supplemental PlaybackInfo must never smuggle signed upstream
				// paths, bearer tokens or temporary sessions into item detail.
				if strings.Contains(rr.Body.String(), "UPSTREAM_SENSITIVE") ||
					strings.Contains(rr.Body.String(), "must-not-leak-session") ||
					strings.Contains(rr.Body.String(), "upstream.example") {
					t.Fatal("detail leaked playback URL, upstream token or session")
				}
				if sources[1]["Name"] != "Higher Quality Cut" {
					t.Fatalf("newly discovered version lacks display name: %v", sources[1]["Name"])
				}
				// Two immediate detail requests are one discovery, not two
				// upstream PlaybackInfo negotiations. A safe short-lived snapshot
				// must still present the recovered second version on the repeat.
				repeated := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+virtual, virtual)
				if repeated.Code != http.StatusOK {
					t.Fatalf("repeated detail status=%d", repeated.Code)
				}
				var repeatedItem map[string]any
				if err := json.Unmarshal(repeated.Body.Bytes(), &repeatedItem); err != nil {
					t.Fatal(err)
				}
				if len(asItems(repeatedItem["MediaSources"])) != 2 || playbackHits.Load() != 1 {
					t.Fatalf("repeated detail should reuse verified metadata, sources=%d upstream probes=%d",
						len(asItems(repeatedItem["MediaSources"])), playbackHits.Load())
				}

			})
		})
	}
}

// When the upstream already supplies both versions in Item detail, do not
// create an unnecessary playback session or extra upstream request.
func TestPhase4SNTPCompleteDetailAvoidsPlaybackProbe(t *testing.T) {
	const itemID = "complete-item"
	full := task6HTTPMovie(itemID, task6HTTPSource("source-a", 1000), task6HTTPSource("source-b", 1000))
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture", "User": map[string]any{"Id": "phase4-user"}})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture"})
		case r.URL.Path == "/Items/"+itemID:
			_ = json.NewEncoder(w).Encode(full)
		case r.URL.Path == "/Items/"+itemID+"/PlaybackInfo":
			probes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": full["MediaSources"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		client := app.Upstream.Clients()[0]
		id, err := app.IDStore.RegisterMergeItem(newMergeCandidate(client.ID, deepCloneMap(full), nil, true), "")
		if err != nil {
			t.Fatal(err)
		}
		rr := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+id, id)
		if rr.Code != http.StatusOK {
			t.Fatalf("detail status %d", rr.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(asItems(out["MediaSources"])) != 2 || probes.Load() != 0 {
			t.Fatalf("complete two-source detail should need no PlaybackInfo probe, sources=%d probes=%d", len(asItems(out["MediaSources"])), probes.Load())
		}
	})
}

// Selecting an exact virtual source must not expand that request into all
// other versions or invoke the unselected on-demand discovery route.
func TestPhase4SNTPSelectedDetailKeepsSingleSource(t *testing.T) {
	const original = "phase4-selected-item"
	first := task6HTTPSource("cut-a", 1000)
	second := task6HTTPSource("cut-b", 1000)
	full := task6HTTPMovie(original, first, second)
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture", "User": map[string]any{"Id": "up-user"}})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture"})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/up-user/Items/"+original:
			// Source selection independently revalidates exact membership.
			_ = json.NewEncoder(w).Encode(full)
		case r.Method == http.MethodGet && r.URL.Path == "/Items/"+original:
			selected := r.URL.Query().Get("MediaSourceId")
			if selected != "cut-b" {
				t.Errorf("explicit virtual source was not translated upstream: %q", selected)
			}
			copy := deepCloneMap(full)
			copy["MediaSources"] = []any{deepCloneMap(second)}
			_ = json.NewEncoder(w).Encode(copy)
		case r.URL.Path == "/Items/"+original+"/PlaybackInfo":
			probes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": full["MediaSources"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		client := app.Upstream.Clients()[0]
		id, err := app.IDStore.RegisterMergeItem(newMergeCandidate(client.ID, deepCloneMap(full), nil, true), "")
		if err != nil {
			t.Fatal(err)
		}
		selected, err := app.IDStore.GetMergeMediaSourceID(client.ID, original, "cut-b")
		if err != nil {
			t.Fatal(err)
		}
		rr := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+id+"?MediaSourceId="+selected, id)
		if rr.Code != http.StatusOK {
			t.Fatalf("selected detail status=%d, body=%s", rr.Code, rr.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		versions := asItems(result["MediaSources"])
		if len(versions) != 1 || versions[0]["Id"] != selected || probes.Load() != 0 {
			t.Fatalf("explicit detail expanded versions: count=%d selected=%q probes=%d", len(versions), selected, probes.Load())
		}
	})
}

// Burst reads of one detail must share the same short-lived discovery rather
// than generating parallel upstream PlaybackInfo requests.
func TestPhase4SNTPConcurrentDetailsCoalesceDiscovery(t *testing.T) {
	const itemID = "phase4-concurrent-item"
	one := task6HTTPSource("cut-a", 1000)
	two := task6HTTPSource("cut-b", 1000)
	full := task6HTTPMovie(itemID, one, two)
	partial := deepCloneMap(full)
	partial["MediaSources"] = []any{deepCloneMap(one)}
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture", "User": map[string]any{"Id": "up-user"}})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture"})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/"+itemID:
			_ = json.NewEncoder(w).Encode(partial)
		case r.Method == http.MethodPost && r.URL.Path == "/Items/"+itemID+"/PlaybackInfo":
			probes.Add(1)
			time.Sleep(120 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": full["MediaSources"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		client := app.Upstream.Clients()[0]
		id, err := app.IDStore.RegisterMergeItem(newMergeCandidate(client.ID, deepCloneMap(partial), nil, false), "")
		if err != nil {
			t.Fatal(err)
		}
		const parallel = 6
		start := make(chan struct{})
		errs := make(chan string, parallel)
		var wg sync.WaitGroup
		for i := 0; i < parallel; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rr := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+id, id)
				if rr.Code != http.StatusOK {
					errs <- "detail status non-200"
					return
				}
				var item map[string]any
				if json.Unmarshal(rr.Body.Bytes(), &item) != nil || len(asItems(item["MediaSources"])) != 2 {
					errs <- "detail did not expose both versions"
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for message := range errs {
			t.Error(message)
		}
		if probes.Load() != 1 {
			t.Fatalf("concurrent details should share 1 probe, got %d", probes.Load())
		}
	})
}

// Failed/unsupported supplemental discovery must retain the usable detail
// response without fabricating a stale version from its saved group members.
func TestPhase4SNTPFailedMetadataProbeDoesNotFabricateVersion(t *testing.T) {
	const itemID = "stale-item"
	full := task6HTTPMovie(itemID, task6HTTPSource("present", 1000), task6HTTPSource("stale", 1000))
	partial := deepCloneMap(full)
	partial["MediaSources"] = []any{deepCloneMap(task6HTTPSource("present", 1000))}
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture", "User": map[string]any{"Id": "phase4-user"}})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture"})
		case r.URL.Path == "/Items/"+itemID:
			_ = json.NewEncoder(w).Encode(partial)
		case r.URL.Path == "/Items/"+itemID+"/PlaybackInfo":
			probes.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, _ http.Handler) {
		client := app.Upstream.Clients()[0]
		virtual, err := app.IDStore.RegisterMergeItem(newMergeCandidate(client.ID, deepCloneMap(full), nil, true), "")
		if err != nil {
			t.Fatal(err)
		}
		rr := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+virtual, virtual)
		if rr.Code != http.StatusOK {
			t.Fatalf("unsupported probe must not break detail: %d", rr.Code)
		}
		var item map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		if len(asItems(item["MediaSources"])) != 1 || probes.Load() > 1 {
			t.Fatalf("failed probe should preserve one verified source only, count=%d probes=%d", len(asItems(item["MediaSources"])), probes.Load())
		}
		if !app.IDStore.MergeMemberAllowed(virtual, client.ID, itemID, "stale") {
			t.Fatal("a partial/failed probe must not delete previously stored memberships")
		}
	})
}
