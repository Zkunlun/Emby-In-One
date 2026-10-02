package backend

import (
	"net/http"
	"testing"
)

func TestPhase6CPlaybackDeviceIdentityPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		headers    http.Header
		capturedID string
		want       string
	}{
		{
			name: "direct header wins over every fallback",
			headers: http.Header{
				"X-Emby-Device-Id":     []string{"direct-device"},
				"X-Emby-Authorization": []string{`MediaBrowser DeviceId="x-auth-device"`},
				"Authorization":        []string{`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "direct-device",
		},
		{
			name: "x emby authorization wins authorization and captured",
			headers: http.Header{
				"X-Emby-Authorization": []string{`MediaBrowser DEVICEID="x-auth-device"`},
				"Authorization":        []string{`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "x-auth-device",
		},
		{
			name: "authorization wins captured",
			headers: http.Header{
				"Authorization": []string{`Emby deviceid="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name:       "captured token identity is last fallback",
			headers:    http.Header{},
			capturedID: " captured-device ",
			want:       "captured-device",
		},
		{
			name: "blank direct header falls through",
			headers: http.Header{
				"X-Emby-Device-Id":     []string{"   "},
				"X-Emby-Authorization": []string{`MediaBrowser DeviceId="x-auth-device"`},
			},
			capturedID: "captured-device",
			want:       "x-auth-device",
		},
		{
			name: "malformed x emby authorization does not block authorization",
			headers: http.Header{
				"X-Emby-Authorization": []string{`MediaBrowser DeviceId="unterminated`},
				"Authorization":        []string{`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "x emby authorization without device does not block authorization",
			headers: http.Header{
				"X-Emby-Authorization": []string{`MediaBrowser Client="Hills"`},
				"Authorization":        []string{`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "ambiguous x emby device does not block lower priority source",
			headers: http.Header{
				"X-Emby-Authorization": []string{`MediaBrowser DeviceId="one", deviceid="two"`},
				"Authorization":        []string{`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "user agent and device name are never playback identity",
			headers: http.Header{
				"User-Agent":         []string{"SomePlayer/1.0"},
				"X-Emby-Device-Name": []string{"Living Room"},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePlaybackDeviceID(tc.headers, tc.capturedID); got != tc.want {
				t.Fatalf("resolvePlaybackDeviceID() = %q, want %q", got, tc.want)
			}
		})
	}
}
