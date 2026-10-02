package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhase5ISessionRoutingFollowsSelectedMediaSource(t *testing.T) {
	store, err := NewIDStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewIDStore: %v", err)
	}
	defer store.Close()

	app := &App{IDStore: store, playbackRoutes: newPlaybackRouteStore()}
	item := store.GetOrCreateVirtualID("item-a", "server-a")
	store.AssociateAdditionalInstance(item, "item-b", "server-b")
	mediaSourceB := store.GetOrCreateVirtualID("ms-b", "server-b")
	playSessionA := store.GetOrCreateVirtualID("play-a", "server-a")
	reqCtx := &RequestContext{
		ProxyToken:       "token-user",
		ProxyUser:        &tokenInfo{UserID: "user-1", Role: "user"},
		PlaybackDeviceID: "xbox-001",
	}
	owner := playbackRouteOwner(reqCtx)
	app.playbackRoutes.RememberMediaSource(owner, mediaSourceB, "server-b", "play-b", playSessionA)

	body := map[string]any{
		"ItemId":        item,
		"MediaSourceId": mediaSourceB,
		"PlaySessionId": playSessionA,
	}
	serverID, found := app.translateSessionBodyIDs(reqCtx, body)
	if !found || serverID != "server-b" {
		t.Fatalf("target server = %q found=%v, want server-b", serverID, found)
	}
	if got := body["ItemId"]; got != "item-b" {
		t.Fatalf("ItemId = %#v, want item-b for selected server", got)
	}
	if got := body["MediaSourceId"]; got != "ms-b" {
		t.Fatalf("MediaSourceId = %#v, want ms-b", got)
	}
	if got := body["PlaySessionId"]; got != "play-b" {
		t.Fatalf("PlaySessionId = %#v, want selected server session play-b", got)
	}
}

func TestPhase5ISessionRoutingUsesRequestScopedActiveRouteWithoutMediaSource(t *testing.T) {
	store, err := NewIDStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewIDStore: %v", err)
	}
	defer store.Close()

	app := &App{IDStore: store, playbackRoutes: newPlaybackRouteStore()}
	item := store.GetOrCreateVirtualID("item-a", "server-a")
	store.AssociateAdditionalInstance(item, "item-b", "server-b")
	playSessionA := store.GetOrCreateVirtualID("play-a", "server-a")
	reqCtx := &RequestContext{ProxyToken: "token-user", ProxyUser: &tokenInfo{UserID: "user-1", Role: "user"}}
	app.playbackRoutes.Activate(playbackRouteOwner(reqCtx), item, "server-b", "play-b", playSessionA)

	body := map[string]any{"ItemId": item, "PlaySessionId": playSessionA}
	serverID, found := app.translateSessionBodyIDs(reqCtx, body)
	if !found || serverID != "server-b" {
		t.Fatalf("target server = %q found=%v, want active server-b", serverID, found)
	}
	if got := body["ItemId"]; got != "item-b" {
		t.Fatalf("ItemId = %#v, want item-b from active route", got)
	}
	if got := body["PlaySessionId"]; got != "play-b" {
		t.Fatalf("PlaySessionId = %#v, want active route session play-b", got)
	}
}

func TestPhase5IStreamSwitchMovesExactDeviceLeaseAfterTargetReserve(t *testing.T) {
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

		reqCtx := &RequestContext{ProxyToken: token, ProxyUser: info, PlaybackDeviceID: "xbox-001"}
		app.playbackRoutes.Activate(playbackRouteOwner(reqCtx), item, "server-a", "play-a", "client-play-a")
		req := httptest.NewRequest(http.MethodGet, "/Videos/"+item+"/stream.mp4", nil)
		req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
		rr := httptest.NewRecorder()

		if !app.switchPlaybackLease(rr, req, "server-b", "server-a", item, "play-b", "play-a", "client-play-a") {
			t.Fatalf("switchPlaybackLease rejected: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("source lease count = %d, want 0", got)
		}
		if got := app.PlaybackLimiter.CountForServer("server-b"); got != 1 {
			t.Fatalf("target lease count = %d, want 1", got)
		}
		key := streamKey{UserID: info.UserID, ServerID: "server-b"}
		app.PlaybackLimiter.mu.Lock()
		entry := app.PlaybackLimiter.streams[key]
		app.PlaybackLimiter.mu.Unlock()
		if entry == nil || entry.DeviceID != "xbox-001" || entry.PlaySessionID != "play-b" {
			t.Fatalf("target lease = %+v, want xbox-001/play-b", entry)
		}
		active, ok := app.playbackRoutes.Active(playbackRouteOwner(reqCtx), item)
		if !ok || active.ServerID != "server-b" || active.PlaySessionID != "play-b" || active.ClientPlaySessionID != "client-play-a" {
			t.Fatalf("active route = %+v ok=%v, want server-b/play-b", active, ok)
		}
	})
}
