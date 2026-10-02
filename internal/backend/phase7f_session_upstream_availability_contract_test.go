package backend

import (
	"net/http"
	"testing"
)

func phase7FRemoveUpstreamClient(app *App, serverID string) {
	app.Upstream.mu.Lock()
	defer app.Upstream.mu.Unlock()
	kept := app.Upstream.clients[:0]
	for _, client := range app.Upstream.clients {
		if client != nil && client.ID == serverID {
			continue
		}
		kept = append(kept, client)
	}
	app.Upstream.clients = kept
}

func TestPhase7FPlayingAndProgressShareUnavailableSemantics(t *testing.T) {
	for _, endpoint := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		for _, availability := range []string{"offline", "missing"} {
			t.Run(endpoint+"/"+availability, func(t *testing.T) {
				upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
				defer upstream.Close()

				withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
					token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
					switch availability {
					case "offline":
						app.Upstream.ClientByID("server-a").setOffline("phase7f offline")
					case "missing":
						phase7FRemoveUpstreamClient(app, "server-a")
					}

					rr := phase7BPostSession(t, handler, endpoint, token, itemID, 900)
					phase7BAssertSessionError(t, rr, http.StatusServiceUnavailable, upstreamSessionUnavailableCode)
					phase7BAssertNoLocalCommit(t, app, info, itemID, before)
				})
			})
		}
	}

}

func TestPhase7FSessionUpstreamClientGate(t *testing.T) {
	upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
		client, available := app.sessionUpstreamClient("server-a")
		if client == nil || !available {
			t.Fatalf("online client gate = (%v, %v), want nonnil/true", client, available)
		}

		client.mu.Lock()
		userID, accessToken := client.UserID, client.AccessToken
		client.UserID = ""
		client.mu.Unlock()
		if client.IsOnline() {
			t.Fatal("general online check accepted missing authentication identity")
		}
		if got, available := app.sessionUpstreamClient("server-a"); got != client || !available {
			t.Fatalf("missing UserID gate = (%v, %v), want client/true for preparation", got, available)
		}
		client.mu.Lock()
		client.UserID = userID
		client.AccessToken = ""
		client.mu.Unlock()
		if got, available := app.sessionUpstreamClient("server-a"); got != client || available {
			t.Fatalf("missing access token gate = (%v, %v), want client/false", got, available)
		}
		client.mu.Lock()
		client.AccessToken = accessToken
		client.mu.Unlock()

		client.setOffline("phase7f offline")
		if got, available := app.sessionUpstreamClient("server-a"); got == nil || available {
			t.Fatalf("offline client gate = (%v, %v), want nonnil/false", got, available)
		}

		phase7FRemoveUpstreamClient(app, "server-a")
		if got, available := app.sessionUpstreamClient("server-a"); got != nil || available {
			t.Fatalf("missing client gate = (%v, %v), want nil/false", got, available)
		}
	})
}
