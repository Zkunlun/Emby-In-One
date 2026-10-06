package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPhase7StoppedTranslationRequiresExplicitRouteSessionMatch(t *testing.T) {
	store, err := NewIDStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := &App{IDStore: store, playbackRoutes: newPlaybackRouteStore()}
	item := store.GetOrCreateVirtualID("item-a", "server-a")
	store.AssociateAdditionalInstance(item, "item-b", "server-b")
	media := store.GetOrCreateVirtualID("ms-b", "server-b")
	currentA := store.GetOrCreateVirtualID("play-current-a", "server-a")
	oldA := store.GetOrCreateVirtualID("play-old-a", "server-a")
	currentB := store.GetOrCreateVirtualID("play-current-b", "server-b")
	oldB := store.GetOrCreateVirtualID("play-old-b", "server-b")
	owner := &RequestContext{ProxyToken: "owner", ProxyUser: &tokenInfo{UserID: "user", Role: "user"}}
	seedPhase5RouteScope(app, owner)
	app.playbackRoutes.RememberMediaSource(playbackRouteOwner(owner), media, "server-b", "play-current-b", currentA)
	app.playbackRoutes.RememberMediaSourceItem(playbackRouteOwner(owner), media, item, "server-b")
	app.playbackRoutes.Activate(playbackRouteOwner(owner), item, "server-b", "play-current-b", currentA)

	for _, routeKind := range []string{"active", "media"} {
		for _, tc := range []struct {
			name        string
			session     string
			otherOwner  bool
			wantServer  string
			wantSession string
		}{
			{name: "current client alias", session: currentA, wantServer: "server-b", wantSession: "play-current-b"},
			{name: "raw current client alias", session: "play-current-a", wantServer: "server-b", wantSession: "play-current-b"},
			{name: "current target virtual", session: currentB, wantServer: "server-b", wantSession: "play-current-b"},
			{name: "current target raw", session: "play-current-b", wantServer: "server-b", wantSession: "play-current-b"},
			{name: "old client session", session: oldA, wantServer: "server-a", wantSession: "play-old-a"},
			{name: "old target session", session: oldB, wantServer: "server-b", wantSession: "play-old-b"},
			{name: "unknown old raw session", session: "unmapped-old", wantServer: "server-b", wantSession: "unmapped-old"},
			{name: "missing session", wantServer: "server-b", wantSession: ""},
			{name: "other token cannot use alias", session: currentA, otherOwner: true, wantServer: "server-a", wantSession: "play-current-a"},
		} {
			t.Run(routeKind+"/"+tc.name, func(t *testing.T) {
				reqCtx := owner
				if tc.otherOwner {
					reqCtx = &RequestContext{ProxyToken: "other", ProxyUser: owner.ProxyUser}
				}
				body := map[string]any{"ItemId": item, "PlaySessionId": tc.session}
				if routeKind == "media" {
					body["MediaSourceId"] = media
				}
				serverID, found := app.translateSessionBodyIDs(reqCtx, body)
				// An explicit B version cannot consume an unproven old/other-owner A session.
				if routeKind == "media" && (tc.name == "old client session" || tc.otherOwner) {
					if found {
						t.Fatalf("conflicting explicit version/session accepted: %#v", body)
					}
					return
				}
				if routeKind == "active" && (tc.name == "missing session" || tc.name == "unknown old raw session") {
					tc.wantServer = "server-a"
				}
				if !found || serverID != tc.wantServer || body["PlaySessionId"] != tc.wantSession {
					t.Fatalf("translation server=%q found=%v session=%v, want %s/%s", serverID, found, body["PlaySessionId"], tc.wantServer, tc.wantSession)
				}
				wantItem := "item-b"
				if tc.wantServer == "server-a" {
					wantItem = "item-a"
				}
				if body["ItemId"] != wantItem {
					t.Fatalf("ItemId=%v want=%s", body["ItemId"], wantItem)
				}
			})
		}
	}
}

func TestPhase7StoppedLatestRouteCannotReleaseNewLease(t *testing.T) {
	for _, routeKind := range []string{"active", "media"} {
		for _, mode := range []string{"success", "rejected", "transport", "offline", "missing", "auth preparation", "URL preparation"} {
			t.Run(routeKind+"/"+mode, func(t *testing.T) {
				upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
				defer upstream.Close()
				withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
					token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
					key := streamKey{UserID: info.UserID, ServerID: "server-a"}
					app.PlaybackLimiter.mu.Lock()
					app.PlaybackLimiter.streams[key].PlaySessionID = "play-new"
					app.PlaybackLimiter.mu.Unlock()
					newSession := app.IDStore.GetOrCreateVirtualID("play-new", "server-a")
					oldSession := app.IDStore.GetOrCreateVirtualID("play-old", "server-a")
					mediaID := app.IDStore.GetOrCreateVirtualID("ms-a", "server-a")
					owner := "token:" + token
					app.playbackRoutes.Activate(owner, itemID, "server-a", "play-new", newSession)
					app.playbackRoutes.RememberMediaSource(owner, mediaID, "server-a", "play-new", newSession)

					var sentSessions []string
					client := app.Upstream.ClientByID("server-a")
					client.httpClient.Transport = phase7BRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						var sent map[string]any
						if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
							return nil, err
						}
						session, _ := sent["PlaySessionId"].(string)
						sentSessions = append(sentSessions, session)
						if mode == "transport" {
							return nil, errors.New("session transport failed")
						}
						status := http.StatusNoContent
						if mode == "rejected" {
							status = http.StatusInternalServerError
						}
						return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
					})
					switch mode {
					case "offline":
						client.setOffline("test offline")
					case "missing":
						phase7FRemoveUpstreamClient(app, "server-a")
					case "auth preparation":
						phase7HClearUpstreamUserIdentity(client)
					case "URL preparation":
						phase7HCorruptUpstreamBaseURL(client)
					}

					postStopped := func(session string) {
						body := map[string]any{
							"ItemId": itemID, "UserId": info.UserID, "PlaySessionId": session,
							"PositionTicks": int64(600), "RunTimeTicks": int64(1000),
						}
						if routeKind == "media" {
							body["MediaSourceId"] = mediaID
						}
						rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", body)
						switch mode {
						case "auth preparation":
							phase7HAssertPreparationError(t, rr)
						case "URL preparation":
							phase7HAssertClientInputPreparationError(t, rr)
						default:
							if rr.Code != http.StatusNoContent {
								t.Fatalf("Stopped status=%d want=204 body=%s", rr.Code, rr.Body.String())
							}
						}
					}

					postStopped(oldSession)
					if got := app.PlaybackLimiter.CountForServer("server-a"); got != 1 {
						t.Fatalf("old Stopped released new lease: count=%d", got)
					}
					if after := phase1ELeaseHeartbeat(t, app, info.UserID, "server-a"); !after.Equal(before) {
						t.Fatalf("old Stopped changed heartbeat: before=%v after=%v", before, after)
					}
					app.PlaybackLimiter.mu.Lock()
					current := app.PlaybackLimiter.streams[key].PlaySessionID
					app.PlaybackLimiter.mu.Unlock()
					if current != "play-new" {
						t.Fatalf("old Stopped changed current session=%q", current)
					}
					progress := app.WatchStore.GetProgress(info.UserID, itemID)
					if progress == nil || progress.PositionTicks != 111 || progress.RuntimeTicks != 1000 {
						t.Fatalf("old Stopped changed current shared progress: %#v", progress)
					}

					seedPhase5WatchOwner(t, app, info.UserID, itemID, "server-a", "item-a", "xbox-001", "play-new", "ms-a", 1000)
					postStopped(newSession)
					if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
						t.Fatalf("current Stopped left matching lease: count=%d", got)
					}
					if mode == "success" || mode == "rejected" || mode == "transport" {
						if len(sentSessions) != 2 || sentSessions[0] != "play-old" || sentSessions[1] != "play-new" {
							t.Fatalf("forwarded sessions=%v want=[play-old play-new]", sentSessions)
						}
					} else if len(sentSessions) != 0 {
						t.Fatalf("unavailable/preparation path reached upstream: %v", sentSessions)
					}
				})
			})
		}
	}
}

func TestPhase7StoppedPlaybackInfoAliasSurvivesStreamSwitch(t *testing.T) {
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
		itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		app.IDStore.AssociateAdditionalInstance(itemID, "item-b", "server-b")
		rr := phase1DPlaybackInfo(t, handler, token, itemID, "xbox-001")
		if rr.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo status=%d body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		clientSession, _ := payload["PlaySessionId"].(string)
		if clientSession == "" {
			t.Fatal("PlaybackInfo omitted client PlaySessionId")
		}
		mediaB := ""
		for _, source := range asItems(map[string]any{"Items": payload["MediaSources"]}) {
			id, _ := source["Id"].(string)
			if resolved := app.IDStore.ResolveVirtualID(id); resolved != nil && resolved.ServerID == "server-b" {
				mediaB = id
			}
		}
		if mediaB == "" {
			t.Fatal("PlaybackInfo omitted server-b MediaSource")
		}
		reqCtx := &RequestContext{ProxyToken: token, ProxyUser: info, PlaybackDeviceID: "xbox-001"}
		owner := playbackRouteOwner(reqCtx)
		mediaRoute, ok := app.playbackRoutes.MediaSource(owner, mediaB)
		if !ok || mediaRoute.ClientPlaySessionID != clientSession || mediaRoute.PlaySessionID != "play-b" {
			t.Fatalf("PlaybackInfo route=%+v ok=%v client session=%q", mediaRoute, ok, clientSession)
		}
		query := url.Values{"MediaSourceId": {mediaB}, "PlaySessionId": {clientSession}}
		req := httptest.NewRequest(http.MethodGet, "/Videos/"+itemID+"/stream.mp4?"+query.Encode(), nil)
		req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
		app.resolvePlaySessionID(query)
		streamResponse := httptest.NewRecorder()
		client, originalItem, selected := app.resolveStreamTarget(streamResponse, req, app.resolveRouteID(itemID), itemID, query)
		if !selected || client == nil || client.ID != "server-b" || originalItem != "item-b" || query.Get("PlaySessionId") != "play-b" {
			t.Fatalf("stream target selected=%v client=%v item=%q query=%v body=%s", selected, client, originalItem, query, streamResponse.Body.String())
		}
		active, ok := app.playbackRoutes.Active(owner, itemID)
		if !ok || active.ServerID != "server-b" || active.ClientPlaySessionID != clientSession {
			t.Fatalf("stream switch lost client session alias: %+v ok=%v", active, ok)
		}
		if app.PlaybackLimiter.CountForServer("server-a") != 0 || app.PlaybackLimiter.CountForServer("server-b") != 1 {
			t.Fatal("stream switch did not move the lease to server-b")
		}
		oldSession := app.IDStore.GetOrCreateVirtualID("old-play-b", "server-b")
		stopped := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
			"ItemId": itemID, "PlaySessionId": oldSession, "PositionTicks": int64(500),
		})
		if stopped.Code != http.StatusNoContent || app.PlaybackLimiter.CountForServer("server-b") != 1 {
			t.Fatalf("old Stopped disturbed switched lease: status=%d leases=%d", stopped.Code, app.PlaybackLimiter.CountForServer("server-b"))
		}
		stopped = phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
			"ItemId": itemID, "PlaySessionId": clientSession, "PositionTicks": int64(600),
		})
		if stopped.Code != http.StatusNoContent || app.PlaybackLimiter.CountForServer("server-b") != 0 {
			t.Fatalf("current client alias did not release switched lease: status=%d leases=%d", stopped.Code, app.PlaybackLimiter.CountForServer("server-b"))
		}
	})
}
