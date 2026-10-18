package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPhase7SingleSourceMediaListErrorDoesNotExposeUpstreamBody(t *testing.T) {
	const upstreamPrivateMarker = "PRIVATE_SOURCE_SQL_ERROR_INCLUDE_CREDENTIAL"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && (r.URL.Path == "/Users/user-a/Items" || r.URL.Path == "/Users/user-a/Items/Latest"):
			http.Error(w, upstreamPrivateMarker, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		container := app.IDStore.GetOrCreateVirtualID("library-a", app.Upstream.Clients()[0].ID)
		proxyUser := app.Auth.ProxyUserID()
		for _, route := range []string{"/Users/" + proxyUser + "/Items", "/Users/" + proxyUser + "/Items/Latest"} {
			t.Run(route, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, route+"?ParentId="+url.QueryEscape(container), nil, token)
				if rr.Code != http.StatusBadGateway {
					t.Fatalf("source500 must remain gateway error: code=%d response=%s", rr.Code, rr.Body.String())
				}
				if strings.Contains(rr.Body.String(), upstreamPrivateMarker) {
					t.Fatalf("raw source diagnostic disclosed to client: %s", rr.Body.String())
				}
			})
		}
	})
}
