package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhase6HStreamSwitchMissingDeviceLeavesLeaseAndRoutesUntouched(t *testing.T) {
	upstreamA := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstreamA.Close()
	upstreamB := phase1ELifecycleUpstream(t, "tok-b", "user-b")
	defer upstreamB.Close()

	withTempAppConfig(t, phase1EDualPlaybackConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}

		item := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		app.IDStore.AssociateAdditionalInstance(item, "item-b", "server-b")
		created := app.PlaybackLimiter.Reserve(info.UserID, "server-a", "xbox-001", item, "play-a")
		if !created.Allowed || !created.Created {
			t.Fatalf("source reserve = %+v, want created", created)
		}

		reqCtx := &RequestContext{ProxyToken: token, ProxyUser: info}
		owner := playbackRouteOwner(reqCtx)
		app.playbackRoutes.Activate(owner, item, "server-a", "play-a", "client-play-a")
		app.IDStore.SetActiveStream(item, "server-a")

		req := httptest.NewRequest(http.MethodGet, "/Videos/"+item+"/stream.mp4", nil)
		req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
		rr := httptest.NewRecorder()

		if app.switchPlaybackLease(rr, req, "server-b", "server-a", item, "play-b", "play-a", "client-play-a") {
			t.Fatal("missing-device stream switch unexpectedly succeeded")
		}
		if rr.Code != http.StatusBadRequest || phase1GErrorCode(t, rr) != playbackDeviceIDRequiredCode {
			t.Fatalf("missing-device stream switch: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 1 {
			t.Fatalf("source lease count=%d, want 1", got)
		}
		if got := app.PlaybackLimiter.CountForServer("server-b"); got != 0 {
			t.Fatalf("target lease count=%d, want 0", got)
		}
		active, ok := app.playbackRoutes.Active(owner, item)
		if !ok || active.ServerID != "server-a" || active.PlaySessionID != "play-a" {
			t.Fatalf("active route mutated: %+v ok=%v, want server-a/play-a", active, ok)
		}
		legacy, ok := app.IDStore.GetActiveStream(item)
		if !ok || legacy != "server-a" {
			t.Fatalf("legacy active stream mutated: server=%q ok=%v, want server-a", legacy, ok)
		}
	})
}
