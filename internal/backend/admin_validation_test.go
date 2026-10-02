package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateTimeoutsRanges(t *testing.T) {
	valid := TimeoutsConfig{
		API: 30000, Global: 15000, Login: 10000, HealthCheck: 10000, HealthInterval: 60000,
		SearchGracePeriod: 3000, MetadataGracePeriod: 3000, LatestGracePeriod: 0,
	}
	if err := validateTimeouts(valid); err != nil {
		t.Fatalf("shipped defaults must validate: %v", err)
	}
	if err := validateTimeouts(TimeoutsConfig{
		API: 1, Global: 1, Login: 1, HealthCheck: 1, HealthInterval: 1,
		SearchGracePeriod: 0, MetadataGracePeriod: 0, LatestGracePeriod: 0,
	}); err != nil {
		t.Fatalf("lower bounds and disabled grace periods must validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*TimeoutsConfig)
		field  string
	}{
		{"api overflows time.Duration", func(c *TimeoutsConfig) { c.API = 9223372036855 }, "timeouts.api"},
		{"api past the cap", func(c *TimeoutsConfig) { c.API = maxTimeoutMillis + 1 }, "timeouts.api"},
		{"api zero", func(c *TimeoutsConfig) { c.API = 0 }, "timeouts.api"},
		{"api negative", func(c *TimeoutsConfig) { c.API = -1 }, "timeouts.api"},
		{"global negative", func(c *TimeoutsConfig) { c.Global = -5 }, "timeouts.global"},
		{"login past the cap", func(c *TimeoutsConfig) { c.Login = maxTimeoutMillis + 1 }, "timeouts.login"},
		{"healthCheck past the cap", func(c *TimeoutsConfig) { c.HealthCheck = maxTimeoutMillis + 1 }, "timeouts.healthCheck"},
		{"healthInterval past the cap", func(c *TimeoutsConfig) { c.HealthInterval = maxIntervalMillis + 1 }, "timeouts.healthInterval"},
		{"search grace negative", func(c *TimeoutsConfig) { c.SearchGracePeriod = -1 }, "timeouts.searchGracePeriod"},
		{"search grace past the cap", func(c *TimeoutsConfig) { c.SearchGracePeriod = maxGraceMillis + 1 }, "timeouts.searchGracePeriod"},
		{"metadata grace past the cap", func(c *TimeoutsConfig) { c.MetadataGracePeriod = maxGraceMillis + 1 }, "timeouts.metadataGracePeriod"},
		{"latest grace past the cap", func(c *TimeoutsConfig) { c.LatestGracePeriod = maxGraceMillis + 1 }, "timeouts.latestGracePeriod"},
	}
	for _, tc := range cases {
		cfg := valid
		tc.mutate(&cfg)
		err := validateTimeouts(cfg)
		if err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q should name %s", tc.name, err.Error(), tc.field)
		}
	}
}

// TestAdminSettingsRejectsOutOfRangeTimeout keeps a mistyped timeout out of the
// config entirely: no partial write, no silently disabled request timeout.
func TestAdminSettingsRejectsOutOfRangeTimeout(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		before := app.ConfigStore.Snapshot().Timeouts

		rr := doJSONRequest(t, handler, http.MethodPut, "/admin/api/settings",
			map[string]any{"timeouts": map[string]any{"api": 999999999999999}}, token)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "timeouts.api") {
			t.Fatalf("error should name the offending field: %s", rr.Body.String())
		}
		if got := app.ConfigStore.Snapshot().Timeouts; got != before {
			t.Fatalf("rejected update changed the config: %+v", got)
		}
	})
}

func TestAdminSettingsAcceptsZeroGracePeriod(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")

		rr := doJSONRequest(t, handler, http.MethodPut, "/admin/api/settings",
			map[string]any{"timeouts": map[string]any{"searchGracePeriod": 0, "metadataGracePeriod": 0}}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		got := app.ConfigStore.Snapshot().Timeouts
		if got.SearchGracePeriod != 0 || got.MetadataGracePeriod != 0 {
			t.Fatalf("zero (disabled) grace periods were not applied: %+v", got)
		}
	})
}

func TestValidateUpstreamDraftPlaybackProxyMatrix(t *testing.T) {
	base := UpstreamConfig{
		Name:         "test-upstream",
		URL:          "https://emby.example",
		Username:     "user",
		Password:     "pass",
		SpoofClient:  "none",
		PlaybackMode: "proxy",
	}
	cases := []struct {
		name         string
		playbackMode string
		proxyID      string
		wantErr      bool
	}{
		{name: "proxy without network proxy", playbackMode: "proxy"},
		{name: "proxy with network proxy", playbackMode: "proxy", proxyID: "proxy-1"},
		{name: "redirect without network proxy", playbackMode: "redirect"},
		{name: "redirect with whitespace proxy id", playbackMode: "redirect", proxyID: "  "},
		{name: "redirect with network proxy", playbackMode: "redirect", proxyID: "proxy-1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft := base
			draft.PlaybackMode = tc.playbackMode
			draft.ProxyID = tc.proxyID
			err := validateUpstreamDraft(draft)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected redirect + proxyId to be rejected")
				}
				if !strings.Contains(err.Error(), "直连播放模式不能使用 HTTP 网络代理") {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestMaxConcurrentRejectsNegativeAdminInput(t *testing.T) {
	negative := -1
	base := UpstreamConfig{
		Name:          "test-upstream",
		URL:           "https://emby.example",
		Username:      "user",
		Password:      "pass",
		SpoofClient:   "none",
		PlaybackMode:  "proxy",
		MaxConcurrent: 3,
	}

	for _, tc := range []struct {
		name     string
		isCreate bool
	}{
		{name: "create", isCreate: true},
		{name: "update", isCreate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := base
			if tc.isCreate {
				draft = UpstreamConfig{
					Name:            base.Name,
					URL:             base.URL,
					Username:        base.Username,
					Password:        base.Password,
					SpoofClient:     base.SpoofClient,
					PlaybackMode:    base.PlaybackMode,
					FollowRedirects: true,
				}
			}
			applyAdminUpstreamInput(&draft, adminUpstreamInput{MaxConcurrent: &negative}, tc.isCreate)
			if draft.MaxConcurrent != -1 {
				t.Fatalf("maxConcurrent = %d after %s input, want -1 preserved for validation", draft.MaxConcurrent, tc.name)
			}
			if err := validateUpstreamDraft(draft); err == nil {
				t.Fatalf("negative maxConcurrent unexpectedly accepted on %s", tc.name)
			}
		})
	}
}

func TestValidateUpstreamDraftAuthMatrix(t *testing.T) {
	base := UpstreamConfig{
		Name:         "test-upstream",
		URL:          "https://emby.example",
		SpoofClient:  "none",
		PlaybackMode: "proxy",
	}
	cases := []struct {
		name     string
		username string
		password string
		apiKey   string
		wantErr  bool
	}{
		{name: "password auth with nonempty password", username: "alice", password: "abc12345"},
		{name: "password auth with empty password", username: "alice", password: ""},
		{name: "missing username and password", wantErr: true},
		{name: "password without username", password: "abc12345", wantErr: true},
		{name: "api key auth", apiKey: "KEY123"},
		{name: "empty api key only", apiKey: "", wantErr: true},
		{name: "username empty password plus api key", username: "alice", apiKey: "KEY123", wantErr: true},
		{name: "username nonempty password plus api key", username: "alice", password: "abc12345", apiKey: "KEY123", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft := base
			draft.Username = tc.username
			draft.Password = tc.password
			draft.APIKey = tc.apiKey
			err := validateUpstreamDraft(draft)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateUpstreamDraft(%q, %q, %q) succeeded, want error", tc.username, tc.password, tc.apiKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateUpstreamDraft(%q, %q, %q) error = %v, want nil", tc.username, tc.password, tc.apiKey, err)
			}
		})
	}
}

func TestApplyAdminUpstreamInputPasswordUpdateSemantics(t *testing.T) {
	strptr := func(v string) *string { return &v }

	base := UpstreamConfig{
		Name:         "test-upstream",
		URL:          "https://emby.example",
		Username:     "alice",
		Password:     "abc12345",
		SpoofClient:  "none",
		PlaybackMode: "proxy",
	}

	t.Run("omitted password keeps existing value", func(t *testing.T) {
		draft := base
		applyAdminUpstreamInput(&draft, adminUpstreamInput{}, false)
		if draft.Password != "abc12345" {
			t.Fatalf("password = %q, want existing password preserved", draft.Password)
		}
	})

	t.Run("nonempty password replaces existing value", func(t *testing.T) {
		draft := base
		applyAdminUpstreamInput(&draft, adminUpstreamInput{Password: strptr("xyz12345")}, false)
		if draft.Password != "xyz12345" {
			t.Fatalf("password = %q, want replacement password", draft.Password)
		}
	})

	t.Run("explicit empty password clears existing value", func(t *testing.T) {
		draft := base
		applyAdminUpstreamInput(&draft, adminUpstreamInput{Password: strptr("")}, false)
		if draft.Password != "" {
			t.Fatalf("password = %q, want explicit empty password", draft.Password)
		}
	})
}

func TestApplyAdminUpstreamInputAuthTypeClearsInactiveCredentials(t *testing.T) {
	strptr := func(v string) *string { return &v }

	t.Run("password auth clears stale api key even with empty password", func(t *testing.T) {
		draft := UpstreamConfig{APIKey: "STALE-KEY"}
		applyAdminUpstreamInput(&draft, adminUpstreamInput{
			AuthType: strptr("password"),
			Username: strptr("alice"),
			Password: strptr(""),
			APIKey:   strptr("SHOULD-NOT-SURVIVE"),
		}, false)
		if draft.Username != "alice" || draft.Password != "" {
			t.Fatalf("password credentials = %q/%q, want alice/empty", draft.Username, draft.Password)
		}
		if draft.APIKey != "" {
			t.Fatalf("apiKey = %q, want inactive credential cleared", draft.APIKey)
		}
	})

	t.Run("apiKey auth clears stale username and password", func(t *testing.T) {
		draft := UpstreamConfig{Username: "old-user", Password: "old-pass"}
		applyAdminUpstreamInput(&draft, adminUpstreamInput{
			AuthType: strptr("apiKey"),
			Username: strptr("SHOULD-NOT-SURVIVE"),
			Password: strptr("SHOULD-NOT-SURVIVE"),
			APIKey:   strptr("KEY123"),
		}, false)
		if draft.APIKey != "KEY123" {
			t.Fatalf("apiKey = %q, want KEY123", draft.APIKey)
		}
		if draft.Username != "" || draft.Password != "" {
			t.Fatalf("inactive username/password = %q/%q, want both cleared", draft.Username, draft.Password)
		}
	})
}

func TestAdminUpstreamRejectsInvalidAuthBoundaries(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "should-not-be-used",
			"User":        map[string]any{"Id": "should-not-be-used"},
		})
	}))
	defer upstream.Close()

	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "password auth missing username with empty password",
			body: map[string]any{"authType": "password", "username": "", "password": ""},
		},
		{
			name: "password auth missing username with nonempty password",
			body: map[string]any{"authType": "password", "username": "", "password": "abc12345"},
		},
		{
			name: "apiKey auth requires nonempty key",
			body: map[string]any{"authType": "apiKey", "apiKey": "", "username": "stale-user", "password": "stale-pass"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempApp(t, func(app *App, handler http.Handler) {
				token := loginToken(t, handler, "secret")
				beforeAttempts := attempts
				body := map[string]any{
					"name":         "invalid-auth",
					"url":          upstream.URL,
					"playbackMode": "proxy",
					"spoofClient":  "none",
				}
				for key, value := range tc.body {
					body[key] = value
				}

				rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream", body, token)
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
				}
				if got := len(app.ConfigStore.Snapshot().Upstream); got != 0 {
					t.Fatalf("invalid auth mutated config: upstream count = %d, want 0", got)
				}
				if attempts != beforeAttempts {
					t.Fatalf("invalid auth reached upstream validation: attempts %d -> %d", beforeAttempts, attempts)
				}
			})
		})
	}
}

func TestAdminUpstreamRejectsRedirectWithProxy(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		before := len(app.ConfigStore.Snapshot().Upstream)

		rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream", map[string]any{
			"name":         "redirect-with-proxy",
			"url":          "https://emby.example",
			"username":     "user",
			"password":     "pass",
			"authType":     "password",
			"playbackMode": "redirect",
			"spoofClient":  "none",
			"proxyId":      "proxy-1",
		}, token)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "直连播放模式不能使用 HTTP 网络代理") {
			t.Fatalf("unexpected error body: %s", rr.Body.String())
		}
		if got := len(app.ConfigStore.Snapshot().Upstream); got != before {
			t.Fatalf("rejected upstream changed config length: got %d want %d", got, before)
		}
	})
}
