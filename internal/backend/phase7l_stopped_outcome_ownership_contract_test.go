package backend

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestPhase7LStoppedOutcomeMatrixFinalizesOnlyExactLease(t *testing.T) {
	for modeIndex, mode := range phase7LModes() {
		for ownerIndex, owner := range []string{"matching", "wrong device", "old session"} {
			for _, route := range []string{"active", "media"} {
				t.Run(mode.Name+"/"+owner+"/"+route, func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						user, server := (modeIndex+ownerIndex)%2, (modeIndex/2+ownerIndex)%2
						device, session := phase7KDeviceID, phase7KSessionID
						body := f.Body(user, server, 700)
						body["UserId"] = f.Users[1-user].Info.UserID
						if route == "active" {
							delete(body, "MediaSourceId")
						}
						if owner == "wrong device" {
							device = "phase7l-other-device"
						}
						if owner == "old session" {
							session = "phase7l-old-session"
							body["PlaySessionId"] = f.App.IDStore.GetOrCreateVirtualID(session, f.Servers[server])
						}
						before := phase7LSnapshot(t, f)
						attempts := phase7LPrepareMode(t, f, server, mode)
						rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[user].Token, device, body)
						phase7LAssertResponse(t, rr, mode)
						key := phase7LWatchOwner(f, user, server)
						phase7LAssertProgress(t, f, before, key, f.Servers[server], phase7KItemID, 700, 2000, false)
						var changedLeases []streamKey
						if owner == "matching" {
							changedLeases = []streamKey{f.Key(user, server)}
							phase7LAssertLeaseAbsent(t, f, changedLeases[0])
						}
						phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{key}, changedLeases)
						phase7LAssertCalls(t, f, server, mode, attempts, session, 700)
					})
				})
			}
		}
	}
}

func TestPhase7LStoppedEmptyMissingAndExpiredLeaseBoundaries(t *testing.T) {
	cases := []struct {
		name                                        string
		leaseSession                                string
		requestSession                              string
		omit, absent, expired, wrongDevice, removed bool
	}{
		{name: "provisional empty exact", removed: true},
		{name: "provisional rejects explicit", requestSession: phase7KSessionID},
		{name: "committed rejects empty", leaseSession: phase7KSessionID},
		{name: "committed rejects omitted", leaseSession: phase7KSessionID, omit: true},
		{name: "committed rejects unknown raw", leaseSession: phase7KSessionID, requestSession: "unknown-old-session"},
		{name: "absent lease is not recreated", requestSession: phase7KSessionID, absent: true, removed: true},
		{name: "expired matching is cleaned", leaseSession: phase7KSessionID, requestSession: phase7KSessionID, expired: true, removed: true},
		{name: "expired wrong device is cleaned", leaseSession: phase7KSessionID, requestSession: "unknown-old-session", expired: true, wrongDevice: true, removed: true},
	}
	for caseIndex, tc := range cases {
		for _, mode := range []phase7LMode{{Name: "HTTP 204", Status: http.StatusNoContent}, {Name: "offline"}, {Name: "auth preparation"}} {
			for _, route := range []string{"active", "media"} {
				t.Run(tc.name+"/"+mode.Name+"/"+route, func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						user, server := caseIndex%2, (caseIndex/2)%2
						leaseKey := f.Key(user, server)
						f.App.PlaybackLimiter.mu.Lock()
						delete(f.App.PlaybackLimiter.streams, leaseKey)
						f.App.PlaybackLimiter.mu.Unlock()
						if !tc.absent {
							result := f.App.PlaybackLimiter.Reserve(leaseKey.UserID, leaseKey.ServerID, phase7KDeviceID, f.Items[server], tc.leaseSession)
							if !result.Allowed || !result.Created {
								t.Fatalf("boundary reserve failed: %+v", result)
							}
							if tc.expired {
								phase1EAgeLease(t, f.App, leaseKey.UserID, leaseKey.ServerID, time.Now().Add(-playbackHeartbeatTimeout-time.Second))
							}
						}
						body := f.Body(user, server, 700)
						body["PlaySessionId"] = tc.requestSession
						if tc.requestSession == phase7KSessionID {
							body["PlaySessionId"] = f.Sessions[server]
						}
						if tc.omit {
							delete(body, "PlaySessionId")
						}
						if route == "active" {
							delete(body, "MediaSourceId")
						}
						device := phase7KDeviceID
						if tc.wrongDevice {
							device = "phase7l-other-device"
						}
						before := phase7LSnapshot(t, f)
						attempts := phase7LPrepareMode(t, f, server, mode)
						rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[user].Token, device, body)
						phase7LAssertResponse(t, rr, mode)
						watchKey := phase7LWatchOwner(f, user, server)
						phase7LAssertProgress(t, f, before, watchKey, leaseKey.ServerID, phase7KItemID, 700, 2000, false)
						var changed []streamKey
						if tc.removed {
							changed = []streamKey{leaseKey}
							phase7LAssertLeaseAbsent(t, f, leaseKey)
						}
						phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{watchKey}, changed)
						phase7LAssertCalls(t, f, server, mode, attempts, tc.requestSession, 700)
						if tc.omit && mode.Status != 0 {
							if _, exists := f.Upstreams[server].Requests()[0].Body["PlaySessionId"]; exists {
								t.Fatal("missing session was manufactured from latest route")
							}
						}
					})
				})
			}
		}
	}
}

func TestPhase7LStopReturnValueSeparatesMatchingReleaseFromExpiryCleanup(t *testing.T) {
	for _, tc := range []struct {
		name                                                               string
		expired, wrongDevice, wrongUser, wrongServer, wrongSession, absent bool
		wantReleased, wantAbsent                                           bool
	}{
		{name: "matching release", wantReleased: true, wantAbsent: true},
		{name: "wrong device", wrongDevice: true},
		{name: "wrong user", wrongUser: true},
		{name: "wrong server", wrongServer: true},
		{name: "wrong session", wrongSession: true},
		{name: "expired cleanup", expired: true, wantAbsent: true},
		{name: "expired mismatched cleanup", expired: true, wrongDevice: true, wrongSession: true, wantAbsent: true},
		{name: "no lease", absent: true, wantAbsent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := NewPlaybackLimiter()
			if !tc.absent {
				limiter.Reserve("user-a", "server-a", "device-a", "item-a", "session-a")
			}
			if tc.expired {
				limiter.mu.Lock()
				limiter.streams[streamKey{UserID: "user-a", ServerID: "server-a"}].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
				limiter.mu.Unlock()
			}
			user, server, device, session := "user-a", "server-a", "device-a", "session-a"
			if tc.wrongUser {
				user = "user-b"
			}
			if tc.wrongServer {
				server = "server-b"
			}
			if tc.wrongDevice {
				device = "device-b"
			}
			if tc.wrongSession {
				session = "session-b"
			}
			var before streamEntry
			if entry := limiter.streams[streamKey{UserID: "user-a", ServerID: "server-a"}]; entry != nil {
				before = *entry
			}
			if released := limiter.Stop(user, server, device, session); released != tc.wantReleased {
				t.Fatalf("released=%v want=%v", released, tc.wantReleased)
			}
			entry, exists := limiter.streams[streamKey{UserID: "user-a", ServerID: "server-a"}]
			if exists == tc.wantAbsent {
				t.Fatalf("exists=%v want absent=%v", exists, tc.wantAbsent)
			}
			if exists && *entry != before {
				t.Fatal("mismatched Stop changed current lease")
			}
		})
	}
}

func TestPhase7LStoppedMissingDevicePersistsProgressWithoutForwardOrRelease(t *testing.T) {
	for user := 0; user < 2; user++ {
		for server := 0; server < 2; server++ {
			t.Run(strconv.Itoa(user)+"/"+strconv.Itoa(server), func(t *testing.T) {
				phase7KWithFixture(t, func(f *phase7KFixture) {
					phase6FClearTokenDeviceIdentity(f.App, f.Users[user].Token)
					before := phase7LSnapshot(t, f)
					rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[user].Token, "", f.Body(user, server, 700))
					if rr.Code != http.StatusBadRequest || phase1GErrorCode(t, rr) != playbackDeviceIDRequiredCode {
						t.Fatalf("missing-device response=%d %s", rr.Code, rr.Body.String())
					}
					key := phase7LWatchOwner(f, user, server)
					phase7LAssertProgress(t, f, before, key, f.Servers[server], phase7KItemID, 700, 2000, false)
					phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{key}, nil)
					if len(f.Upstreams[0].Requests()) != 0 || len(f.Upstreams[1].Requests()) != 0 {
						t.Fatal("unidentified Stopped reached upstream")
					}
				})
			})
		}
	}
}
