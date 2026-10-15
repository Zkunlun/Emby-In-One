package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Listing MediaSources in Fields requests their presence, not their completeness.
// A list with only the first version must not be treated as an authoritative
// one-version inventory in the absence of a source of completeness evidence.
func TestPhase2SNTPPartialListMustNotBeDeclaredComplete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count any
	}{
		{name: "count-omitted"},
		{name: "count-equals-returned", count: float64(1)},
		{name: "count-exceeds-returned", count: float64(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := url.Values{}
			acquisitionClaimsComplete := requestMergeFields(fields)
			item := task6HTTPMovie("movie-a", task6HTTPSource("first-cut", 1000))
			if tc.count != nil {
				item["MediaSourceCount"] = tc.count
			}
			candidate := newMergeCandidate("fixture-sntp", item, nil, acquisitionClaimsComplete)
			if candidate.SourcesComplete {
				t.Fatalf("one-source list falsely claims complete (MediaSourceCount=%v, fullFromRequestedFields=%v)",
					tc.count, acquisitionClaimsComplete)
			}
		})
	}
}

// Existing merge membership is not a guarantee that a later upstream detail
// response repeats every member. An incomplete detail must not silently shrink
// the client-visible version list to only the current response's first source.
// This test intentionally bypasses SNTP's private API and reproduces the
// limited-response shape against the EIO HTTP handler.
// Positive control: source selection must stay exact after the RED defects are
// fixed. Both versions belong to one fake upstream, and an explicit source
// selection must not accidentally return all versions.
func TestPhase2SNTPPlaybackInfoUnselectedAndExplicitSource(t *testing.T) {
	itemID := "sntp-two-source-movie"
	full := task6HTTPMovie(itemID, task6HTTPSource("source-a", 1000), task6HTTPSource("source-b", 1000))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "fixture-upstream-only",
				"User":        map[string]any{"Id": "sntp-test-user"},
			})
		case r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture-sntp"})
		case r.URL.Path == "/Items/"+itemID+"/PlaybackInfo":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			selected := r.URL.Query().Get("MediaSourceId")
			if selected == "" {
				selected, _ = body["MediaSourceId"].(string)
			}
			var sources []any
			for _, source := range asItems(full["MediaSources"]) {
				if selected != "" && source["Id"] != selected {
					continue
				}
				source["DirectStreamUrl"] = "/Videos/" + itemID + "/stream.mp4"
				sources = append(sources, source)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"PlaySessionId": "phase2-session", "MediaSources": sources})
		case r.URL.Path == "/Items/"+itemID || r.URL.Path == "/Users/sntp-test-user/Items/"+itemID:
			_ = json.NewEncoder(w).Encode(full)
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
		sourceAVirtual, err := app.IDStore.GetMergeMediaSourceID(client.ID, itemID, "source-a")
		if err != nil {
			t.Fatal(err)
		}
		requestPlayback := func(query string) map[string]any {
			target := "/Items/" + id + "/PlaybackInfo"
			if query != "" {
				target += "?" + query
			}
			request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader([]byte("{}")))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Emby-Device-Id", "phase2-device")
			request.SetPathValue("itemId", id)
			requestContext := &RequestContext{
				Headers:          request.Header.Clone(),
				ProxyUser:        &tokenInfo{UserID: "phase2-fixture-admin", Role: "admin"},
				PlaybackDeviceID: "phase2-device",
			}
			request = request.WithContext(context.WithValue(request.Context(), requestContextKey{}, requestContext))
			rr := httptest.NewRecorder()
			app.handlePlaybackInfo(rr, request)
			if rr.Code != http.StatusOK {
				t.Fatalf("playback status=%d, body=%s", rr.Code, rr.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			return payload
		}
		all := asItems(requestPlayback("")["MediaSources"])
		if len(all) != 2 {
			t.Fatalf("unselected playback must offer 2 sources, got %d", len(all))
		}
		selected := asItems(requestPlayback("MediaSourceId=" + url.QueryEscape(sourceAVirtual))["MediaSources"])
		if len(selected) != 1 || selected[0]["Id"] != sourceAVirtual {
			t.Fatalf("selected playback should return precisely %s, got %v", sourceAVirtual, selected)
		}
	})
}

func TestPhase2SNTPPreviouslyKnownVersionsSurvivePartialDetail(t *testing.T) {
	for _, mediaType := range []string{"Movie", "Episode"} {
		t.Run(mediaType, func(t *testing.T) {
			itemID := "sntp-item"
			full := task6HTTPMovie(itemID, task6HTTPSource("cut-1", 1000), task6HTTPSource("cut-2", 1000))
			full["Name"] = "Fixture two cuts"
			if mediaType == "Episode" {
				full["Type"] = "Episode"
				full["SeriesId"] = "series-1"
				full["ParentIndexNumber"] = 1
				full["IndexNumber"] = 2
			}
			partial := deepCloneMap(full)
			partial["MediaSources"] = []any{deepCloneMap(task6HTTPSource("cut-1", 1000))}
			delete(partial, "MediaSourceCount")

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"AccessToken": "fixture-upstream-only",
						"User":        map[string]any{"Id": "sntp-test-user"},
					})
				case r.URL.Path == "/System/Info/Public":
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture-sntp"})
				case r.Method == http.MethodGet && (r.URL.Path == "/Items/"+itemID ||
					r.URL.Path == "/Users/sntp-test-user/Items/"+itemID):
					_ = json.NewEncoder(w).Encode(partial)
				case r.Method == http.MethodPost && r.URL.Path == "/Items/"+itemID+"/PlaybackInfo":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"PlaySessionId": "fixture-play-session",
						"MediaSources":  full["MediaSources"],
					})
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
				if !app.IDStore.MergeMemberAllowed(id, client.ID, itemID, "cut-1") ||
					!app.IDStore.MergeMemberAllowed(id, client.ID, itemID, "cut-2") {
					t.Fatal("precondition failed: both source memberships must already exist")
				}
				detail := phase2SNTPAdminRequest(t, app, http.MethodGet, "/Items/"+id, id)
				if detail.Code != http.StatusOK {
					t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
				}
				var payload map[string]any
				if err := json.Unmarshal(detail.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				sources := asItems(payload["MediaSources"])
				if len(sources) != 2 {
					t.Fatalf("%s known-two-source detail lost a version: got %d, want 2", mediaType, len(sources))
				}
				expected := map[string]bool{"cut-1": false, "cut-2": false}
				for _, source := range sources {
					virtual, _ := source["Id"].(string)
					original := app.IDStore.ResolveVirtualID(virtual)
					if original == nil || original.MediaItemID != itemID || original.ServerID != client.ID {
						t.Fatalf("invalid qualified virtual source: %q", virtual)
					}
					if _, ok := expected[original.OriginalID]; ok {
						expected[original.OriginalID] = true
					}
				}
				for original, ok := range expected {
					if !ok {
						t.Errorf("missing expected version %q", original)
					}
				}
			})
		})
	}
}
