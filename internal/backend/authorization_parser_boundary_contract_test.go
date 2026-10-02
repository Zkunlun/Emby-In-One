package backend

import (
	"net/http"
	"testing"
)

func TestAuthorizationParserDuplicateParametersRemainFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"equal exact duplicates", `Emby DeviceId="same", DeviceId="same"`, "same"},
		{"trimmed equal duplicates", `Emby DeviceId=" same ", DeviceId="same"`, "same"},
		{"conflicting exact duplicates", `Emby DeviceId="one", DeviceId="two"`, ""},
		{"conflict cannot be overwritten", `Emby DeviceId="one", DeviceId="two", DeviceId="one"`, ""},
		{"mixed duplicates cannot hide conflict", `Emby DeviceId="one", DEVICEID="two", DEVICEID="one"`, ""},
		{"valid sibling remains usable", `Emby Client="one", Client="two", DeviceId="device"`, "device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceIDFromAuthorizationHeader(tc.header); got != tc.want {
				t.Fatalf("DeviceID=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAuthorizationParserRequiresCommaAfterQuotedValue(t *testing.T) {
	for _, header := range []string{
		`Emby DeviceId="one" DeviceId="two"`,
		`Emby DeviceId="one"Client="client"`,
		`Emby DeviceId="one"junk`,
	} {
		if _, ok := parseAuthorizationIdentityStrict(header); ok {
			t.Fatalf("malformed header accepted: %q", header)
		}
	}
	for _, tc := range []struct{ header, want string }{
		{`Emby DeviceId="one" , Client="client"`, "one"},
		{`Emby DeviceId="one,two", Client="client"`, "one,two"},
		{`Emby DeviceId="one\"two", Client="client"`, "one\"two"},
	} {
		if got := deviceIDFromAuthorizationHeader(tc.header); got != tc.want {
			t.Fatalf("valid header DeviceID=%q want=%q", got, tc.want)
		}
	}
}

func TestAuthorizationParserRejectedDeviceSourceFallsThrough(t *testing.T) {
	for _, header := range []string{
		`Emby DeviceId="one", DeviceId="two"`,
		`Emby DeviceId="one" Client="client"`,
	} {
		headers := http.Header{}
		headers.Set("X-Emby-Authorization", header)
		headers.Set("Authorization", `Emby DeviceId="lower"`)
		if got := resolvePlaybackDeviceID(headers, "token-device"); got != "lower" {
			t.Fatalf("DeviceID=%q want lower valid source", got)
		}
		headers.Del("Authorization")
		if got := resolvePlaybackDeviceID(headers, "token-device"); got != "token-device" {
			t.Fatalf("DeviceID=%q want current token fallback", got)
		}
	}
}

func TestAuthorizationParserAmbiguousTokenDoesNotUseLegacyFallback(t *testing.T) {
	for _, header := range []string{
		`Emby Token="one", Token="two"`,
		`Emby Token="one", TOKEN="two"`,
		`Emby Token="one", Token="two", Token="one"`,
	} {
		if got := extractTokenFromAuthHeader(header); got != "" {
			t.Fatalf("ambiguous Token=%q want empty", got)
		}
	}
	if got := extractTokenFromAuthHeader(`Bearer legacy-prefix Token="legacy-token"`); got != "legacy-token" {
		t.Fatalf("legacy Token=%q want legacy-token", got)
	}
}
