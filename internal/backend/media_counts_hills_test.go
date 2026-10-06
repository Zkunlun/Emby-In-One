package backend

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func TestMediaCountsHillsLanguageMetadata(t *testing.T) {
	for _, path := range []string{"/Items/Counts", "/emby/Items/Counts"} {
		for _, userID := range []string{"", "alice"} {
			for _, languageKey := range []string{"X-Emby-Language", "x-EMBY-lANGUAGE"} {
				t.Run(path+"/user="+userID+"/"+languageKey, func(t *testing.T) {
					app, network := countsTestApp(t)
					handler := app.Handler()
					beforeA, beforeB := app.mediaCounts.cache.read("a"), app.mediaCounts.cache.read("b")
					baseline := countsTestRequest(handler, http.MethodGet, path, "fixture-local-token")
					countsTestAssertResponse(t, baseline, 200, "")
					for _, language := range []string{"zh-cn", "en-US", ""} {
						query := url.Values{
							"X-Emby-Client":         {"Hills"},
							"X-Emby-Client-Version": {"1.9.1"},
							"X-Emby-Device-Name":    {"fixture-device"},
							"X-Emby-Device-Id":      {"fixture-device-id"},
							"X-Emby-Authorization":  {"fixture-client-metadata"},
							"X-Emby-Token":          {"fixture-client-metadata"},
							languageKey:             {language},
						}
						if userID != "" {
							query.Set("UserId", userID)
						}
						rr := countsTestRequest(handler, http.MethodGet, path+"?"+query.Encode(), "fixture-local-token")
						countsTestAssertResponse(t, rr, 200, "")
						if countsTestValue(t, rr) != countsTestValue(t, baseline) || rr.Body.String() != baseline.Body.String() {
							t.Fatal("language metadata changed counts or response schema")
						}
					}
					if network.Load() != 0 || len(app.mediaCounts.wake) != 0 ||
						!reflect.DeepEqual(beforeA, app.mediaCounts.cache.read("a")) ||
						!reflect.DeepEqual(beforeB, app.mediaCounts.cache.read("b")) {
						t.Fatal("language metadata changed cache, scheduled refresh, or contacted an upstream")
					}
				})
			}
		}
	}
}

func TestMediaCountsHillsLanguageGuards(t *testing.T) {
	cases := []struct {
		name, suffix, token string
		status              int
		code                string
	}{
		{"duplicate", "&X-Emby-Language=en-US", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"case duplicate", "&x-emby-language=zh-cn", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"invalid encoding", "&X=%zz", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"favorite", "&IsFavorite=true", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"fields", "&Fields=MovieCount", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"item filter", "&IncludeItemTypes=Movie", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"source", "&ServerId=a", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"page", "&Limit=1", "fixture-local-token", 400, "COUNTS_FILTER_UNSUPPORTED"},
		{"other user", "&UserId=bob", "fixture-local-token", 403, "COUNTS_USER_FORBIDDEN"},
		{"upstream user", "&UserId=real-a", "fixture-local-token", 403, "COUNTS_USER_FORBIDDEN"},
		{"empty user", "&UserId=", "fixture-local-token", 400, "INVALID_COUNTS_QUERY"},
		{"missing auth", "", "", 401, ""},
		{"invalid auth", "", "unknown-fixture", 401, ""},
		{"query metadata is not auth", "&X-Emby-Token=fixture-local-token", "", 401, ""},
		{"auth before filters", "&Fields=MovieCount", "", 401, ""},
	}
	for _, path := range []string{"/Items/Counts", "/emby/Items/Counts"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				app, _ := countsTestApp(t)
				rr := countsTestRequest(app.Handler(), http.MethodGet, path+"?X-Emby-Language=zh-cn"+tc.suffix, tc.token)
				countsTestAssertResponse(t, rr, tc.status, tc.code)
			})
		}
	}
}
