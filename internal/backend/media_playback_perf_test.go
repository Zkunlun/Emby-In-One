package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Two independently authorized copies of one item may be fetched concurrently,
// but the client response and its route/session ownership must be assembled
// deterministically. This exercises the actual PlaybackInfo HTTP handler.
func TestPlaybackInfoFetchesIndependentUpstreamsConcurrently(t *testing.T) {
	var started, replied atomic.Int32
	var firstReplySawBoth atomic.Bool
	upstream := func(item, session, source, upstreamUser string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"AccessToken": "token-" + item,
					"User":        map[string]any{"Id": upstreamUser},
				})
			case r.URL.Path == "/Items/"+item+"/PlaybackInfo":
				if r.URL.Query().Get("UserId") != upstreamUser {
					http.Error(w, "wrong upstream UserId", http.StatusBadRequest)
					return
				}
				started.Add(1)
				time.Sleep(100 * time.Millisecond)
				if replied.Add(1) == 1 {
					firstReplySawBoth.Store(started.Load() == 2)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"PlaySessionId": session,
					"MediaSources":  []map[string]any{{"Id": source, "Container": "mkv"}},
				})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("movie-a", "play-a", "source-a", "user-a")
	defer a.Close()
	b := upstream("movie-b", "play-b", "source-b", "user-b")
	defer b.Close()

	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		item := app.IDStore.GetOrCreateVirtualID("movie-a", app.Upstream.Clients()[0].ID)
		app.IDStore.AssociateAdditionalInstance(item, "movie-b", app.Upstream.Clients()[1].ID)
		token := loginToken(t, handler, "secret")

		rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+item+"/PlaybackInfo", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo HTTP status=%d body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		raw, ok := payload["MediaSources"].([]any)
		if !ok || len(raw) != 2 {
			t.Fatalf("must retain both upstream versions, got %v", payload["MediaSources"])
		}
		if got := started.Load(); got != 2 {
			t.Fatalf("requests reached %d upstreams, want both", got)
		}
		if !firstReplySawBoth.Load() {
			t.Fatal("first upstream replied before the second request began; PlaybackInfo fan-out is serialized")
		}
	})
}

// A quicker second upstream must never change primary source order or borrow
// another upstream's playback session. A failed first source may be skipped,
// but a failed source is never silently substituted for an explicit choice.
func TestPlaybackInfoConcurrentResultsPreserveSourceOrder(t *testing.T) {
	cases := []struct {
		name        string
		firstStatus int
		wantCount   int
		wantOwner   int
	}{
		{"first responds slower", http.StatusOK, 2, 0},
		{"first fails, second succeeds", http.StatusServiceUnavailable, 1, 1},
		{"both succeed", http.StatusOK, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := func(item, source, session, user string, status int, delay time.Duration) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"AccessToken": "token-" + item, "User": map[string]any{"Id": user},
						})
					case r.URL.Path == "/Items/"+item+"/PlaybackInfo":
						time.Sleep(delay)
						if status != http.StatusOK {
							w.WriteHeader(status)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{
							"PlaySessionId": session,
							"MediaSources":  []map[string]any{{"Id": source, "Container": "mkv"}},
						})
					default:
						http.NotFound(w, r)
					}
				}))
			}
			first := upstream("movie-a", "source-a", "session-a", "user-a", tc.firstStatus, 90*time.Millisecond)
			defer first.Close()
			second := upstream("movie-b", "source-b", "session-b", "user-b", http.StatusOK, 10*time.Millisecond)
			defer second.Close()
			withTempAppConfig(t, dualUpstreamConfig(first.URL, second.URL), func(app *App, handler http.Handler) {
				instances := app.Upstream.Clients()
				item := app.IDStore.GetOrCreateVirtualID("movie-a", instances[0].ID)
				app.IDStore.AssociateAdditionalInstance(item, "movie-b", instances[1].ID)
				token := loginToken(t, handler, "secret")
				rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+item+"/PlaybackInfo", nil, token)
				if rr.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
				}
				var data map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
					t.Fatal(err)
				}
				raw, ok := data["MediaSources"].([]any)
				if !ok || len(raw) != tc.wantCount {
					t.Fatalf("MediaSources = %#v, want %d", data["MediaSources"], tc.wantCount)
				}
				firstVirtualID, _ := raw[0].(map[string]any)["Id"].(string)
				firstResolved := app.IDStore.ResolveVirtualID(firstVirtualID)
				if firstResolved == nil || firstResolved.ServerID != instances[tc.wantOwner].ID {
					t.Fatalf("first source resolved to %#v, want server index %d", firstResolved, tc.wantOwner)
				}
				if tc.wantCount == 2 {
					secondVirtualID, _ := raw[1].(map[string]any)["Id"].(string)
					secondResolved := app.IDStore.ResolveVirtualID(secondVirtualID)
					if secondResolved == nil || secondResolved.ServerID != instances[1].ID {
						t.Fatalf("second source resolved to %#v", secondResolved)
					}
				}
			})
		})
	}
}

func TestPlaybackInfoExplicitSourceNeverQueriesAnotherServer(t *testing.T) {
	var playbackA, playbackB atomic.Int32
	upstream := func(item, source, user string, hits *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + item, "User": map[string]any{"Id": user}})
			case r.Method == http.MethodGet && r.URL.Path == "/Users/"+user+"/Items/"+item:
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": item, "MediaSources": []map[string]any{{"Id": source}}})
			case r.URL.Path == "/Items/"+item+"/PlaybackInfo":
				hits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"PlaySessionId": "play-" + item,
					"MediaSources": []map[string]any{{"Id": source, "Container": "mkv"}}})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("movie-a", "source-a", "user-a", &playbackA)
	defer a.Close()
	b := upstream("movie-b", "source-b", "user-b", &playbackB)
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		clients := app.Upstream.Clients()
		item := app.IDStore.GetOrCreateVirtualID("movie-a", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(item, "movie-b", clients[1].ID)
		selection, err := app.IDStore.GetMergeMediaSourceID(clients[1].ID, "movie-b", "source-b")
		if err != nil {
			t.Fatal(err)
		}
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodGet,
			"/Items/"+item+"/PlaybackInfo?MediaSourceId="+selection, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("explicit source status=%d body=%s", rr.Code, rr.Body.String())
		}
		if playbackA.Load() != 0 || playbackB.Load() != 1 {
			t.Fatalf("explicit source requested A=%d B=%d, want A=0 B=1", playbackA.Load(), playbackB.Load())
		}
		forged, err := app.IDStore.GetMergeMediaSourceID(clients[1].ID, "movie-b", "not-in-item")
		if err != nil {
			t.Fatal(err)
		}
		rr = doJSONRequest(t, handler, http.MethodGet,
			"/Items/"+item+"/PlaybackInfo?MediaSourceId="+forged, nil, token)
		if rr.Code == http.StatusOK {
			t.Fatalf("unauthorized forged source succeeded: %s", rr.Body.String())
		}
		if playbackA.Load() != 0 || playbackB.Load() != 1 {
			t.Fatalf("forged source made extra upstream PlaybackInfo requests A=%d B=%d", playbackA.Load(), playbackB.Load())
		}
	})
}
