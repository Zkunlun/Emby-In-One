package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// The Phase 0B SenPlayer capture used /emby/videos/.../stream.mp4 with
// DeviceId, Static, local credentials, and Range. Only synthetic identities
// appear here; this test never depends on production tokens or servers.
func TestSenPlayerStreamRouteCaseContract(t *testing.T) {
	cases := []struct {
		name, segment, extension, mode string
		wantStatus                     int
	}{
		{"redirect canonical video control", "Videos", "mp4", "redirect", http.StatusFound},
		{"redirect lowercase video", "videos", "mp4", "redirect", http.StatusFound},
		{"redirect mixed-case video", "ViDeOs", "mp4", "redirect", http.StatusFound},
		{"redirect lowercase audio", "audio", "mp3", "redirect", http.StatusFound},
		{"proxy canonical video control", "Videos", "mp4", "proxy", http.StatusPartialContent},
		{"proxy lowercase video", "videos", "mp4", "proxy", http.StatusPartialContent},
		{"proxy lowercase audio", "audio", "mp3", "proxy", http.StatusPartialContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const upstreamToken = "fixture-upstream-token"
			var streamHits atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"AccessToken": upstreamToken,
						"User":        map[string]any{"Id": "fixture-upstream-user"},
					})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream."+tc.extension):
					streamHits.Add(1)
					// Simulate an upstream rejecting the noncanonical lowercase
					// playback path or mismatched Hills query identity.
					expected := "/" + map[string]string{"mp4": "Videos", "mp3": "Audio"}[tc.extension] +
						"/47187/stream." + tc.extension
					if r.URL.Path != expected ||
						r.URL.Query().Get("DeviceId") != "hills-spoof-id" ||
						r.Header.Get("X-Emby-Device-Id") != "hills-spoof-id" ||
						r.Header.Get("X-Emby-Token") != upstreamToken {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "video/mp4")
					w.Header().Set("Content-Range", "bytes 0-3/16")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, "DATA")
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)

			config := singleUpstreamConfig(upstream.URL) +
				fmt.Sprintf("    playbackMode: %q\n    spoofClient: \"hills\"\n", tc.mode)
			withTempAppConfig(t, config, func(app *App, handler http.Handler) {
				upstreamClient := app.Upstream.GetClient(0)
				if _, err := app.UserStore.Create("watcher", "watcher-pass", []string{upstreamClient.ID}); err != nil {
					t.Fatalf("create allowed viewer: %v", err)
				}
				if _, err := app.UserStore.Create("ungranted", "ungranted-pass", nil); err != nil {
					t.Fatalf("create viewer without access: %v", err)
				}
				token := loginTokenAs(t, handler, "watcher", "watcher-pass")
				noGrantToken := loginTokenAs(t, handler, "ungranted", "ungranted-pass")
				itemID := app.IDStore.GetOrCreateVirtualID("47187", upstreamClient.ID)
				mediaPath := "/emby/" + tc.segment + "/" + itemID + "/stream." + tc.extension
				query := url.Values{
					"DeviceId": {"senplayer-fixture-device"},
					"Static":   {"true"},
					"api_key":  {token},
				}

				// An unauthenticated request must not reach the upstream stream,
				// even through the lowercase fallback route.
				before := streamHits.Load()
				unauthorized := doJSONRequest(t, handler, http.MethodGet, mediaPath, nil, "")
				if unauthorized.Code != http.StatusUnauthorized || streamHits.Load() != before {
					t.Fatalf("unauthenticated playback = %d, stream hits=%d -> %d",
						unauthorized.Code, before, streamHits.Load())
				}
				denied := doJSONRequest(t, handler, http.MethodGet, mediaPath, nil, noGrantToken)
				if denied.Code < http.StatusBadRequest || streamHits.Load() != before {
					t.Fatalf("ungranted viewer got status=%d and %d upstream stream hits",
						denied.Code, streamHits.Load()-before)
				}

				req := httptest.NewRequest(http.MethodGet, mediaPath+"?"+query.Encode(), nil)
				req.Header.Set("X-Emby-Token", token)
				req.Header.Set("User-Agent", "SenPlayer/1.0.0")
				req.Header.Set("Range", "bytes=0-3")
				req.Header.Set("X-Emby-Authorization",
					compoundAuthHeader("", "", "SenPlayer", "Phone",
						"senplayer-fixture-device", "1.0.0"))
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)

				if rr.Code != tc.wantStatus {
					t.Fatalf("%s %s status = %d, want %d",
						tc.mode, tc.segment, rr.Code, tc.wantStatus)
				}
				canonical := "/" + map[string]string{"mp4": "Videos", "mp3": "Audio"}[tc.extension] +
					"/47187/stream." + tc.extension
				if tc.mode == "redirect" {
					if streamHits.Load() != before {
						t.Fatalf("redirect should not proxy media to upstream, hits=%d", streamHits.Load()-before)
					}
					loc, err := url.Parse(rr.Header().Get("Location"))
					if err != nil {
						t.Fatalf("redirect Location parse: %v", err)
					}
					if loc.Path != canonical || loc.Query().Get("api_key") != upstreamToken {
						t.Fatalf("redirect target path=%q, upstream token correctly substituted=%t",
							loc.Path, loc.Query().Get("api_key") == upstreamToken)
					}
					if strings.Contains(loc.String(), token) {
						t.Fatal("redirect leaked EIO local token")
					}
				} else {
					if rr.Body.String() != "DATA" || streamHits.Load() <= before {
						t.Fatalf("proxy response body=%q, upstream hits=%d", rr.Body.String(), streamHits.Load()-before)
					}
				}
			})
		})
	}
}

// An identity profile must not send conflicting device IDs to the upstream.
// Lowercase paths were observed going through the generic (non-stream) request
// outlet; both stream=false and stream=true must recognize playback endpoints.
func TestSenPlayerOutboundPlaybackDeviceIdentityCaseContract(t *testing.T) {
	const (
		localDevice   = "senplayer-fixture-device"
		hillsDevice   = "hills-spoof-id"
		upstreamToken = "fixture-upstream-token"
	)
	observed := make(chan outboundClientHeaderRecord, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- recordClientHeaders(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := newUpstreamClient(Config{}, UpstreamConfig{
		URL: upstream.URL, SpoofClient: "hills",
	}, 0, nil)
	client.setOnline(upstreamToken, "fixture-upstream-user")

	cases := []struct {
		name, path            string
		stream, rewriteDevice bool
	}{
		{"canonical video control", "/Videos/47187/stream.mp4", false, true},
		{"lowercase video fallback", "/videos/47187/stream.mp4", false, true},
		{"mixed video fallback", "/ViDeOs/47187/stream.mp4", false, true},
		{"lowercase video stream", "/videos/47187/stream.mp4", true, true},
		{"canonical audio control", "/Audio/47187/stream.mp3", false, true},
		{"lowercase audio fallback", "/audio/47187/stream.mp3", false, true},
		{"unrelated image control", "/Items/47187/Images/Primary", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := url.Values{
				"DeviceId":         {localDevice},
				"X-Emby-Device-Id": {localDevice},
				"MediaSourceId":    {"source-47187"},
				"PlaySessionId":    {"session-47187"},
				"Static":           {"true"},
				"api_key":          {"fixture-local-token"},
			}
			headers := http.Header{
				"Range":            {"bytes=0-3"},
				"User-Agent":       {"SenPlayer/1.0.0"},
				"X-Emby-Device-Id": {localDevice},
				"X-Emby-Authorization": {compoundAuthHeader(
					"", "", "SenPlayer", "Phone", localDevice, "1.0.0")},
			}
			resp, err := client.doRequest(context.Background(), nil, http.MethodGet,
				tc.path, params, nil, headers, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			got := <-observed

			wantDevice := localDevice
			if tc.rewriteDevice {
				wantDevice = hillsDevice
			}
			if got.query.Get("DeviceId") != wantDevice {
				t.Errorf("outbound DeviceId = %q, want %q", got.query.Get("DeviceId"), wantDevice)
			}
			if got.query.Get("X-Emby-Device-Id") != hillsDevice ||
				got.headers.Get("X-Emby-Device-Id") != hillsDevice {
				t.Errorf("Hills spoof query/header identity inconsistent: %q/%q",
					got.query.Get("X-Emby-Device-Id"), got.headers.Get("X-Emby-Device-Id"))
			}
			if compound, ok := parseAuthorizationIdentityStrict(got.headers.Get("X-Emby-Authorization")); !ok ||
				compound["DeviceId"] != hillsDevice {
				t.Errorf("upstream compound identity not Hills: valid=%t", ok)
			}
			if got.headers.Get("X-Emby-Token") != upstreamToken {
				t.Error("upstream token not substituted in header")
			}
			if got.headers.Get("User-Agent") == "SenPlayer/1.0.0" {
				t.Error("real SenPlayer user agent leaked")
			}
			for key, value := range map[string]string{
				"MediaSourceId": "source-47187", "PlaySessionId": "session-47187", "Static": "true",
			} {
				if got.query.Get(key) != value {
					t.Errorf("business parameter %s changed", key)
				}
			}
			if got.headers.Get("Range") != "bytes=0-3" {
				t.Error("Range header changed")
			}
			if got.path != tc.path {
				t.Errorf("identity-only outbound path changed: got %q, want %q", got.path, tc.path)
			}
			if tc.stream {
				if got.query.Get("api_key") != upstreamToken {
					t.Error("stream query did not carry upstream token")
				}
			} else if got.query.Has("api_key") {
				t.Error("non-stream query retained a credential")
			}
		})
	}
}
