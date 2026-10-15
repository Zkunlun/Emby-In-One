package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestDirectStreamPreservesExactUpstreamPath is the RED contract for an upstream
// whose DirectStreamUrl is not the canonical /Videos/{id}/stream.ext shape.
// EIO may keep exposing its own canonical client URL, but when that URL comes
// back it must recover the exact upstream business path learned from PlaybackInfo.
func TestDirectStreamPreservesExactUpstreamPath(t *testing.T) {
	const (
		upstreamItemID  = "episode-1"
		upstreamMSID    = "ms-1"
		upstreamPlayID  = "play-1"
		upstreamToken   = "upstream-token"
		upstreamUserID  = "user-a"
		exactStreamPath = "/videos/episode-1/original.mkv"
	)

	type observedStream struct {
		path          string
		mediaSourceID string
		playSessionID string
		apiKey        string
		rangeHeader   string
	}
	var mu sync.Mutex
	var observed observedStream
	var genericObservedPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": upstreamToken,
				"User":        map[string]any{"Id": upstreamUserID},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/"+upstreamItemID+"/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources": []map[string]any{{
					"Id":        upstreamMSID,
					"ItemId":    upstreamItemID,
					"Container": "mkv",
					"DirectStreamUrl": exactStreamPath + "?MediaSourceId=" + upstreamMSID +
						"&PlaySessionId=" + upstreamPlayID + "&DeviceId=fixture-device&api_key=" + upstreamToken,
				}},
				"PlaySessionId": upstreamPlayID,
			})
		case r.Method == http.MethodGet && (r.URL.Path == exactStreamPath || strings.Contains(r.URL.Path, "/stream.mkv")):
			mu.Lock()
			observed = observedStream{
				path:          r.URL.Path,
				mediaSourceID: r.URL.Query().Get("MediaSourceId"),
				playSessionID: r.URL.Query().Get("PlaySessionId"),
				apiKey:        r.URL.Query().Get("api_key"),
				rangeHeader:   r.Header.Get("Range"),
			}
			mu.Unlock()
			if r.URL.Path != exactStreamPath {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"invalid_grant"}`))
				return
			}
			w.Header().Set("Content-Type", "video/x-matroska")
			w.Header().Set("Content-Range", "bytes 0-3/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("DATA"))
		case r.Method == http.MethodGet && r.URL.Path == "/Videos/"+upstreamItemID+"/download.mkv":
			mu.Lock()
			genericObservedPath = r.URL.Path
			mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("GENERIC"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"A\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n", upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		proxyToken := loginToken(t, handler, "secret")
		virtualItemID := app.IDStore.GetOrCreateVirtualID(upstreamItemID, app.Upstream.Clients()[0].ID)

		playbackRR := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtualItemID+"/PlaybackInfo", nil, proxyToken)
		if playbackRR.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo status = %d, body=%s", playbackRR.Code, playbackRR.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(playbackRR.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode PlaybackInfo: %v", err)
		}
		mediaSources := asItems(map[string]any{"Items": payload["MediaSources"]})
		if len(mediaSources) != 1 {
			t.Fatalf("MediaSources count = %d, payload=%#v", len(mediaSources), payload)
		}
		directURL, _ := mediaSources[0]["DirectStreamUrl"].(string)
		if directURL == "" {
			t.Fatalf("PlaybackInfo missing DirectStreamUrl: %#v", mediaSources[0])
		}
		if strings.Contains(directURL, upstream.URL) || strings.Contains(directURL, upstreamToken) {
			t.Fatalf("client-facing DirectStreamUrl leaked upstream authority or token: %q", directURL)
		}
		parsedClientURL, err := url.Parse(directURL)
		if err != nil {
			t.Fatalf("parse client DirectStreamUrl %q: %v", directURL, err)
		}
		wantClientPath := "/Videos/" + virtualItemID + "/stream.mkv"
		if parsedClientURL.Path != wantClientPath {
			t.Fatalf("client DirectStreamUrl path = %q, want canonical EIO path %q", parsedClientURL.Path, wantClientPath)
		}
		clientQuery := parsedClientURL.Query()
		if clientQuery.Get("api_key") != proxyToken {
			t.Fatalf("client DirectStreamUrl api_key = %q, want local proxy token", clientQuery.Get("api_key"))
		}
		if clientQuery.Get("MediaSourceId") == "" || clientQuery.Get("MediaSourceId") == upstreamMSID {
			t.Fatalf("client MediaSourceId was not virtualized: %q", clientQuery.Get("MediaSourceId"))
		}
		if clientQuery.Get("PlaySessionId") == "" || clientQuery.Get("PlaySessionId") == upstreamPlayID {
			t.Fatalf("client PlaySessionId was not virtualized: %q", clientQuery.Get("PlaySessionId"))
		}

		streamReq := httptest.NewRequest(http.MethodGet, directURL, nil)
		streamReq.Header.Set("Range", "bytes=0-3")
		streamRR := httptest.NewRecorder()
		handler.ServeHTTP(streamRR, streamReq)

		mu.Lock()
		got := observed
		mu.Unlock()
		if got.path != exactStreamPath {
			t.Fatalf("upstream DirectStream path = %q, want exact PlaybackInfo path %q", got.path, exactStreamPath)
		}
		if got.mediaSourceID != upstreamMSID {
			t.Fatalf("upstream MediaSourceId = %q, want %q", got.mediaSourceID, upstreamMSID)
		}
		if got.playSessionID != upstreamPlayID {
			t.Fatalf("upstream PlaySessionId = %q, want %q", got.playSessionID, upstreamPlayID)
		}
		if got.apiKey != upstreamToken {
			t.Fatalf("upstream api_key = %q, want upstream token", got.apiKey)
		}
		if got.rangeHeader != "bytes=0-3" {
			t.Fatalf("upstream Range = %q, want bytes=0-3", got.rangeHeader)
		}
		if streamRR.Code != http.StatusPartialContent {
			t.Fatalf("stream status = %d, want 206, body=%s", streamRR.Code, streamRR.Body.String())
		}
		if streamRR.Header().Get("Content-Range") != "bytes 0-3/8" {
			t.Fatalf("Content-Range = %q, want bytes 0-3/8", streamRR.Header().Get("Content-Range"))
		}
		if streamRR.Body.String() != "DATA" {
			t.Fatalf("stream body = %q, want DATA", streamRR.Body.String())
		}

		// The exact-path route is scoped to the canonical DirectStream rest only. A
		// different rest for the same authorized source must keep the legacy generic path.
		genericURL := "/Videos/" + virtualItemID + "/download.mkv?" + parsedClientURL.RawQuery
		genericReq := httptest.NewRequest(http.MethodGet, genericURL, nil)
		genericRR := httptest.NewRecorder()
		handler.ServeHTTP(genericRR, genericReq)
		mu.Lock()
		gotGenericPath := genericObservedPath
		mu.Unlock()
		if gotGenericPath != "/Videos/"+upstreamItemID+"/download.mkv" {
			t.Fatalf("generic stream path = %q, want legacy path /Videos/%s/download.mkv", gotGenericPath, upstreamItemID)
		}
		if genericRR.Code != http.StatusOK || genericRR.Body.String() != "GENERIC" {
			t.Fatalf("generic stream response = %d %q, want 200 GENERIC", genericRR.Code, genericRR.Body.String())
		}
	})
}

func TestDirectStreamTargetPath(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		streamBase string
		want       string
	}{
		{
			name:       "relative exact path",
			raw:        "/videos/episode-1/original.mkv?api_key=secret",
			streamBase: "https://upstream.example",
			want:       "/videos/episode-1/original.mkv",
		},
		{
			name:       "strip configured deployment prefix only",
			raw:        "https://cdn.example/emby/videos/episode-1/original.mkv?api_key=secret",
			streamBase: "https://upstream.example/emby",
			want:       "/videos/episode-1/original.mkv",
		},
		{
			name:       "do not strip prefix lookalike",
			raw:        "/embypass/videos/episode-1/original.mkv",
			streamBase: "https://upstream.example/emby",
			want:       "/embypass/videos/episode-1/original.mkv",
		},
		{
			name:       "preserve escaped path",
			raw:        "/emby/videos/My%20Film/original.mkv?api_key=secret",
			streamBase: "https://upstream.example/emby",
			want:       "/videos/My%20Film/original.mkv",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := directStreamTargetPath(tt.raw, tt.streamBase); got != tt.want {
				t.Fatalf("directStreamTargetPath(%q, %q) = %q, want %q", tt.raw, tt.streamBase, got, tt.want)
			}
		})
	}
}
