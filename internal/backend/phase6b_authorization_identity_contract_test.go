package backend

import "testing"

func TestPhase6BAuthorizationIdentityParameterIsCaseInsensitive(t *testing.T) {
	parsed, ok := parseAuthorizationIdentityStrict(`MediaBrowser client="Hills", DEVICEID="xbox-001", tOkEn="token-001"`)
	if !ok {
		t.Fatal("strict compound authorization parser rejected valid mixed-case parameters")
	}
	if got := authorizationIdentityParameter(parsed, "Client"); got != "Hills" {
		t.Fatalf("Client = %q, want Hills", got)
	}
	if got := authorizationIdentityParameter(parsed, "DeviceId"); got != "xbox-001" {
		t.Fatalf("DeviceId = %q, want xbox-001", got)
	}
	if got := authorizationIdentityParameter(parsed, "Token"); got != "token-001" {
		t.Fatalf("Token = %q, want token-001", got)
	}

	ambiguous := map[string]string{"DEVICEID": "xbox-001", "deviceid": "phone-001"}
	if got := authorizationIdentityParameter(ambiguous, "DeviceId"); got != "" {
		t.Fatalf("conflicting DeviceId case variants = %q, want fail-closed empty value", got)
	}
	ambiguousWithCanonical := map[string]string{"DeviceId": "xbox-001", "deviceid": "phone-001"}
	if got := authorizationIdentityParameter(ambiguousWithCanonical, "DeviceId"); got != "" {
		t.Fatalf("canonical plus conflicting DeviceId variant = %q, want fail-closed empty value", got)
	}
}

func TestPhase6BDeviceIDFromAuthorizationHeaderUsesStrictParser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   string
	}{
		{name: "media browser mixed case", header: `MediaBrowser Client="Hills", deviceid="xbox-001", Version="2.0"`, want: "xbox-001"},
		{name: "emby uppercase", header: `Emby DEVICEID="phone-001", Client="Emby"`, want: "phone-001"},
		{name: "missing", header: `MediaBrowser Client="Hills"`, want: ""},
		{name: "malformed quoted value", header: `MediaBrowser DeviceId="unterminated`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceIDFromAuthorizationHeader(tc.header); got != tc.want {
				t.Fatalf("deviceIDFromAuthorizationHeader() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPhase6BTokenExtractionSharesCompoundParserAndKeepsLegacyFallback(t *testing.T) {
	if got := extractTokenFromAuthHeader(`MediaBrowser Client="Hills", TOKEN="strict-token"`); got != "strict-token" {
		t.Fatalf("mixed-case strict Token = %q, want strict-token", got)
	}
	// This is not a normal Emby compound header, but the previous extractor accepted
	// its Token marker. Keep that compatibility while valid headers use the strict parser.
	if got := extractTokenFromAuthHeader(`Bearer legacy-prefix Token="legacy-token"`); got != "legacy-token" {
		t.Fatalf("legacy Token fallback = %q, want legacy-token", got)
	}
}
