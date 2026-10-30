package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A client disconnect must cancel the upstream fan-out without waiting for
// the original per-upstream API timeout.
func TestPhase2PlaybackInfoCancelStopsFanout(t *testing.T) {
	entered := make(chan struct{}, 2)
	upstream := func(item, user string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + item, "User": map[string]any{"Id": user}})
			case r.URL.Path == "/Items/"+item+"/PlaybackInfo":
				entered <- struct{}{}
				<-r.Context().Done()
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("movie-a", "user-a")
	defer a.Close()
	b := upstream("movie-b", "user-b")
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		clients := app.Upstream.Clients()
		item := app.IDStore.GetOrCreateVirtualID("movie-a", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(item, "movie-b", clients[1].ID)
		token := loginToken(t, handler, "secret")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/Items/"+item+"/PlaybackInfo", nil).WithContext(ctx)
		req.Header.Set("X-Emby-Token", token)
		done := make(chan struct{})
		go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), req) }()
		for i := 0; i < 2; i++ {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("two upstreams did not start")
			}
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancel did not release handler")
		}
	})
}

func TestPhase2PlaybackInfoPostKeepsDistinctUpstreamUsers(t *testing.T) {
	var aHits, bHits atomic.Int32
	upstream := func(item, user string, hits *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + item, "User": map[string]any{"Id": user}})
			case r.Method == http.MethodPost && r.URL.Path == "/Items/"+item+"/PlaybackInfo":
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					http.Error(w, "bad body", 400)
					return
				}
				if payload["UserId"] != user || payload["MaxStreamingBitrate"] == nil || r.URL.Query().Has("UserId") {
					http.Error(w, "wrong user or payload", 400)
					return
				}
				hits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"PlaySessionId": "session-" + item, "MediaSources": []map[string]any{{"Id": "source-" + item, "Container": "mkv"}}})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("movie-a", "user-a", &aHits)
	defer a.Close()
	b := upstream("movie-b", "user-b", &bHits)
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		clients := app.Upstream.Clients()
		item := app.IDStore.GetOrCreateVirtualID("movie-a", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(item, "movie-b", clients[1].ID)
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodPost, "/Items/"+item+"/PlaybackInfo", map[string]any{"UserId": "eio-client", "MaxStreamingBitrate": 24000000}, token)
		if rr.Code != 200 {
			t.Fatalf("POST returned %d: %s", rr.Code, rr.Body.String())
		}
		if aHits.Load() != 1 || bHits.Load() != 1 {
			t.Fatalf("upstreams used incorrect POST identity: A=%d B=%d", aHits.Load(), bHits.Load())
		}
	})
}
