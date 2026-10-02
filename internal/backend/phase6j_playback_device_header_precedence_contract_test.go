package backend

import (
	"net/http"
	"testing"
)

func TestPhase6JPlaybackDeviceHeaderPrecedenceMatrix(t *testing.T) {
	tests := []struct {
		name       string
		headers    http.Header
		capturedID string
		want       string
	}{
		{
			name: "direct header wins all lower sources",
			headers: http.Header{
				"X-Emby-Device-Id":     {" direct-device "},
				"X-Emby-Authorization": {`MediaBrowser DeviceId="x-auth-device"`},
				"Authorization":        {`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "direct-device",
		},
		{
			name: "blank direct falls through to x emby authorization",
			headers: http.Header{
				"X-Emby-Device-Id":     {" \t "},
				"X-Emby-Authorization": {`MediaBrowser dEvIcEiD="x-auth-device"`},
				"Authorization":        {`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "x-auth-device",
		},
		{
			name: "x emby authorization wins authorization",
			headers: http.Header{
				"X-Emby-Authorization": {`MediaBrowser Client="Hills", DeviceId="x-auth-device", Version="1.0"`},
				"Authorization":        {`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "x-auth-device",
		},
		{
			name: "malformed x emby authorization falls through to authorization",
			headers: http.Header{
				"X-Emby-Authorization": {`MediaBrowser DeviceId="unterminated`},
				"Authorization":        {`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "x emby authorization without device falls through to authorization",
			headers: http.Header{
				"X-Emby-Authorization": {`MediaBrowser Client="Hills", Version="1.0"`},
				"Authorization":        {`Emby DEVICEID="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "conflicting x emby device variants fall through to authorization",
			headers: http.Header{
				"X-Emby-Authorization": {`MediaBrowser DeviceId="one", deviceid="two"`},
				"Authorization":        {`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "equal x emby device case variants remain unambiguous",
			headers: http.Header{
				"X-Emby-Authorization": {`MediaBrowser DeviceId="same-device", deviceid="same-device"`},
				"Authorization":        {`Emby DeviceId="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "same-device",
		},
		{
			name: "authorization wins token captured fallback",
			headers: http.Header{
				"Authorization": {`Emby Client="Emby", deviceid="auth-device"`},
			},
			capturedID: "captured-device",
			want:       "auth-device",
		},
		{
			name: "malformed authorization falls through to captured token device",
			headers: http.Header{
				"Authorization": {`Emby DeviceId="unterminated`},
			},
			capturedID: " captured-device ",
			want:       "captured-device",
		},
		{
			name: "authorization without device falls through to captured token device",
			headers: http.Header{
				"Authorization": {`Emby Client="Emby", Version="1.0"`},
			},
			capturedID: " captured-device ",
			want:       "captured-device",
		},
		{
			name:       "captured token device is trimmed",
			headers:    http.Header{},
			capturedID: " \t captured-device \n ",
			want:       "captured-device",
		},
		{
			name: "unrelated headers never become playback identity",
			headers: http.Header{
				"User-Agent":         {"Player/1.0"},
				"X-Emby-Device-Name": {"Living Room"},
				"X-Forwarded-For":    {"192.0.2.10"},
			},
			want: "",
		},
		{
			name:       "all sources absent returns empty",
			headers:    http.Header{},
			capturedID: "",
			want:       "",
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

func TestPhase6JAuthorizationDeviceParameterContract(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "quoted value with surrounding parameters",
			header: `MediaBrowser Client="Hills", Device="Xbox", DeviceId="xbox-001", Version="1.0"`,
			want:   "xbox-001",
		},
		{
			name:   "mixed case parameter",
			header: `Emby dEvIcEiD="phone-001", Client="Emby"`,
			want:   "phone-001",
		},
		{
			name:   "unquoted value",
			header: `MediaBrowser DeviceId=xbox-002, Client=Hills`,
			want:   "xbox-002",
		},
		{
			name:   "value whitespace trimmed",
			header: `Emby DeviceId=   phone-002   , Client=Emby`,
			want:   "phone-002",
		},
		{
			name:   "same value case variants accepted",
			header: `MediaBrowser DeviceId="same-device", DEVICEID="same-device"`,
			want:   "same-device",
		},
		{
			name:   "conflicting case variants rejected",
			header: `MediaBrowser DeviceId="xbox", deviceid="phone"`,
			want:   "",
		},
		{
			name:   "missing device parameter",
			header: `MediaBrowser Client="Hills", Device="Xbox"`,
			want:   "",
		},
		{
			name:   "malformed quoted value rejected",
			header: `MediaBrowser DeviceId="unterminated`,
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceIDFromAuthorizationHeader(tc.header); got != tc.want {
				t.Fatalf("deviceIDFromAuthorizationHeader() = %q, want %q", got, tc.want)
			}
		})
	}
}
