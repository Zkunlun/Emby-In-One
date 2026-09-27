package backend

import (
	"net/http"
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
