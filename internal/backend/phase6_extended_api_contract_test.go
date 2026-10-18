package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func phase6TwoSourceConfig(sourceA, sourceB string) string {
	return fmt.Sprintf(`server:
  port: 8096
  name: "Test"
  id: "phase6-proxy"
admin:
  username: "admin"
  password: "secret"
playback:
  mode: "proxy"
timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 60000
proxies: []
upstream:
  - id: "source-a"
    name: "Source A"
    url: %q
    username: "u1"
    password: "p1"
  - id: "source-b"
    name: "Source B"
    url: %q
    username: "u2"
    password: "p2"
`, sourceA, sourceB)
}

func TestPhase6ImageTransientUpstreamStatusNeverPubliclyCached(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantCache bool
	}{
		{"image success", http.StatusOK, true},
		{"upstream image 500", http.StatusInternalServerError, false},
		{"upstream image 404", http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
					_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
				case r.Method == http.MethodGet && r.URL.Path == "/Items/item-a/Images/Primary":
					w.Header().Set("Content-Type", "image/jpeg")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte("image-fixture"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
				token := loginToken(t, handler, "secret")
				virtualID := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
				rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtualID+"/Images/Primary?api_key="+url.QueryEscape(token), nil, "")
				if rr.Code != tc.status {
					t.Fatalf("image status=%d want=%d body=%s", rr.Code, tc.status, rr.Body.String())
				}
				cache := rr.Header().Get("Cache-Control")
				if tc.wantCache && !strings.Contains(cache, "max-age=86400") {
					t.Fatalf("successful image cache=%q", cache)
				}
				if !tc.wantCache && strings.Contains(cache, "public") {
					t.Fatalf("failed image publicly cacheable: status=%d cache=%q", tc.status, cache)
				}
			})
		})
	}
}

func TestPhase6SimilarUpstreamErrorIsNotExposedToClient(t *testing.T) {
	const marker = "VERY_PRIVATE_UPSTREAM_DIAGNOSTIC"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/item-a/Similar":
			http.Error(w, marker, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		itemID := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+itemID+"/Similar", nil, token)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("upstream500 should be 502, got=%d", rr.Code)
		}
		if strings.Contains(rr.Body.String(), marker) {
			t.Fatalf("upstream private diagnostic leaked to client: %s", rr.Body.String())
		}
	})
}

func TestPhase6FallbackTaxonomyParentIDCaseRoutingAndAmbiguity(t *testing.T) {
	var queried string
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && (r.URL.Path == "/Years" || r.URL.Path == "/Tags" || r.URL.Path == "/OfficialRatings"):
			queried = r.URL.Query().Get("parentid")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Name": "A"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-b", "User": map[string]any{"Id": "user-b"}})
		} else {
			http.NotFound(w, r)
		}
	}))
	defer upstreamB.Close()
	withTempAppConfig(t, phase6TwoSourceConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		virtualParent := app.IDStore.GetOrCreateVirtualID("parent-a", "source-a")
		for _, resource := range []string{"Years", "Tags", "OfficialRatings"} {
			t.Run(resource, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, "/"+resource+"?parentid="+url.QueryEscape(virtualParent), nil, token)
				if rr.Code != http.StatusOK {
					t.Fatalf("%s lower-case parent ID status=%d body=%s", resource, rr.Code, rr.Body.String())
				}
				if queried != "parent-a" {
					t.Fatalf("%s lower-case ID not translated: got %q", resource, queried)
				}
			})
		}
		t.Run("truly unconstrained multiserver remains ambiguous", func(t *testing.T) {
			rr := doJSONRequest(t, handler, http.MethodGet, "/Years", nil, token)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("unconstrained multi-server should preserve 400, got=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	})
}

func TestPhase6ThemeMediaUpstreamErrorIsNotExposedToClient(t *testing.T) {
	const marker = "PRIVATE_THEME_MEDIA_RESPONSE"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "theme-token", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/item-a/ThemeMedia":
			http.Error(w, marker, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		itemID := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+itemID+"/ThemeMedia", nil, token)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("upstream500 should remain 502: %d", rr.Code)
		}
		if strings.Contains(rr.Body.String(), marker) {
			t.Fatalf("upstream private theme error leaked: %s", rr.Body.String())
		}
	})
}
