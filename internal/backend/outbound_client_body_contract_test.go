package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func decodeClientBodyNumbers(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func assertClientBodyJSON(t *testing.T, raw string, want map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeClientBodyNumbers(t, raw); !reflect.DeepEqual(got, decodeClientBodyNumbers(t, string(encoded))) {
		t.Fatalf("body changed:\ngot  %s\nwant %s", raw, encoded)
	}
}

func TestOutboundClientBodyModelWire(t *testing.T) {
	// Model fields follow the fixed official API reference. UserId on lifecycle
	// payloads and identity-like private extensions are separate compatibility cases.
	profile := map[string]any{
		"Name": "Original device profile", "Id": "profile-id",
		"DirectPlayProfiles":  []any{map[string]any{"Container": "mkv,mp4", "Type": "Video", "VideoCodec": "hevc,h264"}},
		"TranscodingProfiles": []any{map[string]any{"Container": "ts", "Type": "Video", "Protocol": "hls", "VideoCodec": "h264"}},
		"CodecProfiles":       []any{map[string]any{"Type": "Video", "Codec": "hevc", "Conditions": []any{map[string]any{"Property": "VideoBitDepth", "Condition": "LessThanEqual", "Value": "10"}}}},
	}
	cases := []struct {
		name, path string
		mode       outboundAuthMode
		body       map[string]any
		mapUser    bool
	}{
		{"login", "/Users/AuthenticateByName", authModePasswordLogin, map[string]any{"Username": "u", "Pw": ""}, false},
		{"reauth-login", "/Users/AuthenticateByName", authModePasswordLogin, map[string]any{"Username": "u", "Pw": ""}, false},
		{"playbackinfo", "/Items/item/PlaybackInfo", authModeNormal, map[string]any{
			"UserId": "LOCAL-USER", "MediaSourceId": "media-source", "CurrentPlaySessionId": "current-session",
			"DeviceProfile": profile, "EnableDirectPlay": true, "EnableDirectStream": true, "EnableTranscoding": false,
			"AudioStreamIndex": json.Number("0"), "SubtitleStreamIndex": json.Number("-1"),
			"StartTimeTicks": json.Number("9007199254740993"), "MaxStreamingBitrate": json.Number("9223372036854775807")}, true},
		{"playing", "/Sessions/Playing", authModeNormal, map[string]any{
			"ItemId": "item", "MediaSourceId": "media-source", "PlaySessionId": "play-session", "PlayMethod": "DirectStream",
			"PositionTicks": json.Number("9007199254740993"), "CanSeek": true, "IsPaused": false, "IsMuted": false,
			"PlaybackRate": json.Number("1.2500000000000001"), "NowPlayingQueue": []any{map[string]any{"Id": "queue-item", "PlaylistItemId": "playlist-item"}}}, false},
		{"progress", "/Sessions/Playing/Progress", authModeNormal, map[string]any{
			"ItemId": "item", "MediaSourceId": "media-source", "PlaySessionId": "play-session",
			"PositionTicks": json.Number("9007199254740993"), "PlaybackRate": json.Number("1.2500000000000001"),
			"IsPaused": true, "VolumeLevel": json.Number("0")}, false},
		{"stopped", "/Sessions/Playing/Stopped", authModeNormal, map[string]any{
			"ItemId": "item", "MediaSourceId": "media-source", "PlaySessionId": "play-session",
			"PositionTicks": json.Number("9007199254740993"), "Failed": false, "IsAutomated": false}, false},
		{"capabilities", "/Sessions/Capabilities/Full", authModeNormal, map[string]any{
			"DeviceProfile": profile, "PlayableMediaTypes": []string{"Video", "Audio"}, "SupportedCommands": []string{"Play", "Pause", "Seek"},
			"SupportsMediaControl": true, "SupportsSync": false, "AppId": "original-app", "IconUrl": "https://example.test/icon.png",
			"PushToken": "original-push-token", "PushTokenType": "original-push-type"}, false},
		{"display-preferences", "/DisplayPreferences/settings", authModeNormal, map[string]any{
			"Client": "settings-namespace", "Id": "settings-id", "SortBy": "SortName", "SortOrder": "Ascending",
			"CustomPrefs": map[string]any{"Client": "nested-namespace", "Version": "prefs-version", "DeviceId": "prefs-target", "UserId": "nested-user"}}, false},
		{"device-options", "/Devices/Options", authModeNormal, map[string]any{"CustomName": "Original target device name"}, false},
		{"lifecycle-userid-compatibility", "/Sessions/Playing/Progress", authModeNormal, map[string]any{
			"UserId": "LOCAL-USER", "ItemId": "item", "PositionTicks": json.Number("9007199254740993"),
			"Nested": map[string]any{"UserId": "nested-user", "Client": "nested-client"}}, true},
		{"unknown-private-extension", "/Plugins/PrivateOperation", authModeNormal, map[string]any{
			"UserId": "target-user", "UA": "original-json-ua", "UserAgent": "original-useragent", "Client": "original-client",
			"Version": "original-version", "Device": "original-device", "DeviceName": "original-device-name", "DeviceId": "original-device-id",
			"Nested": map[string]any{"DeviceId": "nested-target", "UserId": "nested-user"}}, false},
	}
	realWant := map[string]string{"User-Agent": "REAL-UA", "X-Emby-Client": "REAL-CLIENT",
		"X-Emby-Client-Version": "REAL-VERSION", "X-Emby-Device-Name": "REAL-DEVICE", "X-Emby-Device-Id": "REAL-ID"}
	for _, mode := range []string{"infuse", "custom", "passthrough"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				records := make(chan outboundClientHeaderRecord, 1)
				stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					records <- recordClientHeaders(r)
					_, _ = io.WriteString(w, "{}")
				}))
				defer stub.Close()
				cfg := UpstreamConfig{URL: stub.URL, SpoofClient: mode}
				wantHeaders := infuseClientHeaderWant()
				if mode == "custom" {
					cfg.CustomUserAgent = "Custom/1"
					cfg.CustomClient = "Custom"
					cfg.CustomDeviceId = "custom-stable"
					wantHeaders = map[string]string{"User-Agent": "Custom/1", "X-Emby-Client": "Custom", "X-Emby-Device-Id": "custom-stable"}
				} else if mode == "passthrough" {
					wantHeaders = realWant
				}
				client := newUpstreamClient(Config{}, cfg, 0, nil)
				if tc.name != "login" {
					client.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
				}
				headers := http.Header{}
				for key, value := range realWant {
					headers.Set(key, value)
				}
				headers.Set("X-Emby-Token", "LOCAL-TOKEN")
				before, _ := json.Marshal(tc.body)
				resp, err := client.doRequestForMode(context.Background(), nil, http.MethodPost, tc.path,
					url.Values{"Id": {"target-device"}}, tc.body, headers, false, tc.mode)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				got := <-records
				want := make(map[string]any, len(tc.body))
				for k, v := range tc.body {
					want[k] = v
				}
				if tc.mapUser {
					want["UserId"] = "UPSTREAM-USER"
				}
				assertClientBodyJSON(t, got.body, want)
				user, token := "UPSTREAM-USER", "UPSTREAM-TOKEN"
				if tc.mode == authModePasswordLogin {
					token = ""
					if tc.name == "login" {
						user = ""
					}
				}
				assertClientHeaders(t, got.headers, wantHeaders, user, token)
				if got.query.Get("Id") != "target-device" || got.headers.Get("Content-Type") != "application/json" {
					t.Fatal("business query/content type changed")
				}
				after, _ := json.Marshal(tc.body)
				if string(before) != string(after) {
					t.Fatal("source body mutated")
				}
			})
		}
	}
}

func TestOutboundClientBodyRawWire(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	client := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "infuse"}, 0, nil)
	client.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	rawObject := " \n{\"PositionTicks\":9007199254740993,\"DeviceProfile\":{\"Name\":\"Original\",\"Id\":\"original-id\"}} \n"
	cases := []struct {
		name, path, contentType, raw string
		pointer, reject              bool
	}{
		{"known-absent-user", "/Items/item/PlaybackInfo", "application/json; charset=utf-8", rawObject, false, false},
		{"known-same-user", "/Sessions/Playing", "application/json", " \n{\"UserId\":\"UPSTREAM-USER\",\"PositionTicks\":9007199254740993} \n", false, false},
		{"raw-pointer", "/Sessions/Playing/Progress", "application/json", rawObject, true, false},
		{"unknown-extension", "/Plugins/PrivateOperation", "application/json", " { \"UserId\":\"target\", \"DeviceId\":\"original\", \"Client\":\"original\", \"PositionTicks\":9007199254740993 } ", false, false},
		{"array-root", "/Sessions/Playing", "application/json", " \n[9007199254740993, {\"Client\":\"original\"}] \n", false, false},
		{"scalar-root", "/Sessions/Playing", "application/json", " 9007199254740993 ", false, false},
		{"string-root", "/Sessions/Playing", "application/json", " \"Original DeviceId\" ", false, false},
		{"null-root", "/Sessions/Playing", "application/json", " null \n", false, false},
		{"empty-body", "/Sessions/Playing", "application/json", "", false, false},
		{"binary", "/Sessions/Playing", "application/octet-stream", " \x00\xff\x01DeviceId=original\r\n ", false, false},
		{"form", "/Sessions/Playing", "application/x-www-form-urlencoded", " DeviceId=original&UserId=local ", false, false},
		{"unknown-malformed", "/Plugins/PrivateOperation", "application/json", "{\"broken\":", false, false},
		{"known-malformed", "/Sessions/Playing", "application/json", "{\"broken\":", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := rawRequestBody{data: []byte(tc.raw), contentType: tc.contentType}
			var body any = raw
			if tc.pointer {
				body = &raw
			}
			resp, err := client.doRequest(context.Background(), nil, http.MethodPost, tc.path, nil, body, nil, false)
			if tc.reject {
				if err == nil {
					if resp != nil {
						_ = resp.Body.Close()
					}
					t.Fatal("existing JSON preparation rejection bypassed")
				}
				select {
				case <-records:
					t.Fatal("invalid known JSON reached upstream")
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			got := <-records
			if got.body != tc.raw || got.headers.Get("Content-Type") != tc.contentType {
				t.Fatalf("raw body/content type changed: %q %q", got.body, got.headers.Get("Content-Type"))
			}
			assertClientHeaders(t, got.headers, infuseClientHeaderWant(), "UPSTREAM-USER", "UPSTREAM-TOKEN")
			if string(raw.data) != tc.raw || raw.contentType != tc.contentType {
				t.Fatal("source raw body mutated")
			}
		})
	}
}

func TestOutboundClientBodyExistingRawUserIDBoundary(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	client := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "infuse"}, 0, nil)
	client.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	raw := rawRequestBody{data: []byte(`{"UserId":"LOCAL-USER","PositionTicks":9007199254740993,"DeviceId":"private-extension-id","Nested":{"UserId":"nested-user","Name":"original"}}`), contentType: "application/json"}
	resp, err := client.doRequest(context.Background(), nil, http.MethodPost, "/Sessions/Playing/Progress", nil, raw, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := <-records
	body := decodeClientBodyNumbers(t, got.body)
	if body["UserId"] != "UPSTREAM-USER" || body["DeviceId"] != "private-extension-id" ||
		!reflect.DeepEqual(body["Nested"], map[string]any{"UserId": "nested-user", "Name": "original"}) {
		t.Fatalf("existing body boundary changed: %s", got.body)
	}
	// Observe the already-documented decoder limitation; do not require rounding
	// as a permanent behavior or confuse it with a new identity transformation.
	t.Logf("existing raw UserId preparation: source PositionTicks=9007199254740993, wire PositionTicks=%v", body["PositionTicks"])
	assertClientHeaders(t, got.headers, infuseClientHeaderWant(), "UPSTREAM-USER", "UPSTREAM-TOKEN")
	if !strings.Contains(string(raw.data), "\"UserId\":\"LOCAL-USER\"") {
		t.Fatal("source raw body mutated")
	}
}

func TestOutboundClientBodyHandlers(t *testing.T) {
	for _, playbackMode := range []string{"proxy", "redirect"} {
		t.Run(playbackMode, func(t *testing.T) {
			records := make(chan outboundClientHeaderRecord, 12)
			newStub := func(label string) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
						_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "TOKEN-" + label, "User": map[string]any{"Id": "USER-" + label}})
						return
					}
					// Background statistics/health do not carry handler JSON bodies.
					if r.URL.Path == "/Items/Counts" || r.URL.Path == "/System/Info/Public" {
						_ = json.NewEncoder(w).Encode(map[string]any{})
						return
					}
					if r.Method == http.MethodGet && r.URL.Path == "/Users/USER-"+label+"/Items/item-a" {
						_ = json.NewEncoder(w).Encode(map[string]any{"Id": "item-a", "MediaSources": []any{map[string]any{"Id": "ms-a"}}})
						return
					}
					records <- recordClientHeaders(r)
					if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
						_ = json.NewEncoder(w).Encode(map[string]any{"PlaySessionId": "play-a", "MediaSources": []any{map[string]any{"Id": "ms-a", "Container": "mp4"}}})
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
			}
			a, b := newStub("A"), newStub("B")
			defer a.Close()
			defer b.Close()
			config := fmt.Sprintf("server:\n  port: 8096\n  name: Test\n  id: svr\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: %s\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: A\n    url: %q\n    username: u\n    password: p\n    spoofClient: infuse\n  - name: B\n    url: %q\n    username: u\n    password: p\n    spoofClient: custom\n    customUserAgent: B-UA\n    customClient: B-CLIENT\n    customClientVersion: B-VERSION\n    customDeviceName: B-DEVICE\n    customDeviceId: B-ID\n", playbackMode, a.URL, b.URL)
			withTempAppConfig(t, config, func(app *App, handler http.Handler) {
				token := loginToken(t, handler, "secret")
				virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
				virtualMS := app.IDStore.GetOrCreateVirtualID("ms-a", app.Upstream.Clients()[0].ID)
				virtualPlay := app.IDStore.GetOrCreateVirtualID("play-a", app.Upstream.Clients()[0].ID)
				deviceProfile := map[string]any{"Name": "Original Profile", "Id": "profile-id",
					"DirectPlayProfiles":  []any{map[string]any{"Container": "mkv,mp4", "Type": "Video", "VideoCodec": "hevc"}},
					"TranscodingProfiles": []any{map[string]any{"Container": "ts", "Type": "Video", "Protocol": "hls"}},
					"PrivateSettings":     map[string]any{"Client": "namespace", "DeviceId": "settings-target"}}
				send := func(path string, body map[string]any, status int) {
					t.Helper()
					encoded, _ := json.Marshal(body)
					before := string(encoded)
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(before))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Emby-Token", token)
					req.Header.Set("User-Agent", "REAL-UA")
					req.Header.Set("X-Emby-Client", "REAL-CLIENT")
					req.Header.Set("X-Emby-Client-Version", "REAL-VERSION")
					req.Header.Set("X-Emby-Device-Name", "REAL-DEVICE")
					req.Header.Set("X-Emby-Device-Id", "REAL-LOCAL-DEVICE")
					headers := req.Header.Clone()
					query := req.URL.RawQuery
					rr := httptest.NewRecorder()
					handler.ServeHTTP(rr, req)
					if rr.Code != status {
						t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
					}
					after, _ := json.Marshal(body)
					if string(after) != before || !reflect.DeepEqual(req.Header, headers) || req.URL.RawQuery != query {
						t.Fatal("source body/header/query mutated")
					}
				}
				assertRecord := func(got outboundClientHeaderRecord, want map[string]any) {
					t.Helper()
					headers := infuseClientHeaderWant()
					user, upToken := "USER-A", "TOKEN-A"
					if got.headers.Get("X-Emby-Token") == "TOKEN-B" {
						headers = map[string]string{"User-Agent": "B-UA", "X-Emby-Client": "B-CLIENT",
							"X-Emby-Client-Version": "B-VERSION", "X-Emby-Device-Name": "B-DEVICE", "X-Emby-Device-Id": "B-ID"}
						user, upToken = "USER-B", "TOKEN-B"
					}
					assertClientHeaders(t, got.headers, headers, user, upToken)
					assertClientBodyJSON(t, got.body, want)
				}
				capabilities := map[string]any{"DeviceProfile": deviceProfile, "PlayableMediaTypes": []string{"Video", "Audio"},
					"SupportedCommands": []string{"Play", "Seek"}, "SupportsMediaControl": true,
					"AppId": "original-app", "IconUrl": "https://example.test/original.png", "PushToken": "original-push"}
				for _, path := range []string{"/Sessions/Capabilities", "/Sessions/Capabilities/Full"} {
					send(path+"?Id=target-session&X-Emby-Client=REAL-QUERY", capabilities, http.StatusNoContent)
					seen := map[string]bool{}
					for i := 0; i < 2; i++ {
						got := <-records
						assertRecord(got, capabilities)
						seen[got.headers.Get("X-Emby-Token")] = true
						if path == "/Sessions/Capabilities" {
							if got.query.Get("Id") != "target-session" || got.query.Get("X-Emby-Client") != got.headers.Get("X-Emby-Client") {
								t.Fatal("capabilities query preparation changed")
							}
						} else if got.query.Get("Id") != "" || got.query.Get("X-Emby-Client") != "" {
							t.Fatal("Full started forwarding previously ignored query")
						}
					}
					if !seen["TOKEN-A"] || !seen["TOKEN-B"] {
						t.Fatal("capabilities broadcast targets changed")
					}
				}
				playback := map[string]any{"UserId": app.Auth.ProxyUserID(), "MediaSourceId": virtualMS, "DeviceProfile": deviceProfile,
					"EnableDirectPlay": true, "EnableTranscoding": false, "MaxStreamingBitrate": 120000000, "StartTimeTicks": 123456789}
				send("/Items/"+virtualItem+"/PlaybackInfo?UserId="+app.Auth.ProxyUserID()+"&X-Emby-Device-Id=REAL-QUERY", playback, http.StatusOK)
				wantPlayback := map[string]any{}
				for k, v := range playback {
					wantPlayback[k] = v
				}
				wantPlayback["UserId"] = "USER-A"
				wantPlayback["MediaSourceId"] = "ms-a"
				got := <-records
				assertRecord(got, wantPlayback)
				if got.query.Get("UserId") != "USER-A" || got.query.Get("X-Emby-Device-Id") != "infuse-spoof-id" {
					t.Fatal("PlaybackInfo query mapping changed")
				}
				for i, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
					body := map[string]any{"ItemId": virtualItem, "MediaSourceId": virtualMS, "PlaySessionId": virtualPlay,
						"UserId": app.Auth.ProxyUserID(), "PositionTicks": 10000 + i, "IsPaused": false,
						"PrivateSettings": map[string]any{"Client": "namespace", "DeviceId": "settings-target"}}
					send(path, body, http.StatusNoContent)
					want := map[string]any{}
					for k, v := range body {
						want[k] = v
					}
					want["ItemId"] = "item-a"
					want["MediaSourceId"] = "ms-a"
					want["PlaySessionId"] = "play-a"
					want["UserId"] = "USER-A"
					assertRecord(<-records, want)
				}
			})
		})
	}
}
