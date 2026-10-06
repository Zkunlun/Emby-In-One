package backend

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMediaCountsRoundBudgetAndReachability(t *testing.T) {
	now := time.Unix(1700000000, 0)
	success := countsAttempt{Sent: true, Success: true, Status: 200, Value: mediaCounts{1, 2, 3}}
	failure := countsAttempt{Sent: true, Status: 500, Class: countsHTTP}
	online := countsAttempt{Sent: true, Success: true, Status: 200}
	limited := countsAttempt{Sent: true, Status: 429, Class: countsLimited, Wait: countsWait{Limited: true, NeedsFallback: true}}
	cases := []struct {
		name          string
		fetches       []countsAttempt
		check         countsAttempt
		calls, checks int
		ok            bool
		reach         countsReachability
		limited       bool
	}{
		{"first success", []countsAttempt{success}, countsAttempt{}, 1, 0, true, countsReachabilityUnknown, false},
		{"failure online retry", []countsAttempt{failure, success}, online, 2, 1, true, countsReachabilityOnline, false},
		{"retry fails", []countsAttempt{failure, failure}, online, 2, 1, false, countsReachabilityOnline, false},
		{"offline no retry", []countsAttempt{failure}, countsAttempt{Sent: true, DefinitelyUnreachable: true}, 1, 1, false, countsReachabilityOffline, false},
		{"unknown no retry", []countsAttempt{failure}, countsAttempt{Sent: true, Status: 403, Class: countsHTTP}, 1, 1, false, countsReachabilityUnknown, false},
		{"first limit no retry", []countsAttempt{limited}, online, 1, 1, false, countsReachabilityOnline, true},
		{"probe limited", []countsAttempt{failure}, limited, 1, 1, false, countsReachabilityUnknown, true},
		{"second limited", []countsAttempt{failure, limited}, online, 2, 1, false, countsReachabilityOnline, true},
		{"preparation zero budget", []countsAttempt{{Class: countsNotReady}}, countsAttempt{}, 0, 0, false, countsReachabilityUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := countsTestClient("a")
			source := prepareCountsSource(client, nil, "", [32]byte{1})
			index, checkCalls := 0, 0
			collector := &countsUpstreamCollector{now: func() time.Time { return now },
				fetch: func(context.Context, countsSourceSpec) countsAttempt {
					if index >= len(tc.fetches) {
						t.Fatal("extra Counts")
					}
					got := tc.fetches[index]
					index++
					return got
				},
				check: func(context.Context, countsSourceSpec) countsAttempt { checkCalls++; return tc.check },
			}
			got := collector.collect(context.Background(), source)
			if got.CountsCalls != tc.calls || got.CheckCalls != tc.checks || checkCalls != tc.checks || got.Success != tc.ok || got.Reachability != tc.reach || got.Wait.Limited != tc.limited {
				t.Fatalf("budget/outcome %+v", got)
			}
			if got.Success && got.Value != (mediaCounts{1, 2, 3}) {
				t.Fatal("incomplete value")
			}
		})
	}
}

func TestMediaCountsRoundStopsOnCancellationOrChangedAccount(t *testing.T) {
	for _, cancelRound := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed-account", true: "canceled"}[cancelRound], func(t *testing.T) {
			client := countsTestClient("a")
			source := prepareCountsSource(client, nil, "", [32]byte{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checkCalls := 0
			collector := &countsUpstreamCollector{
				fetch: func(context.Context, countsSourceSpec) countsAttempt {
					if cancelRound {
						cancel()
					} else {
						client.mu.Lock()
						client.UserID = "other-real-user"
						client.mu.Unlock()
					}
					return countsAttempt{Sent: true, Class: countsHTTP, Status: 500}
				},
				check: func(context.Context, countsSourceSpec) countsAttempt {
					checkCalls++
					return countsAttempt{Sent: true, Success: true}
				},
			}
			result := collector.collect(ctx, source)
			if result.Success || checkCalls != 0 || result.CountsCalls != 1 {
				t.Fatal("continued invalid round")
			}
		})
	}
	source := prepareCountsSource(countsTestClient("a"), nil, "", [32]byte{})
	source.Ready = false
	collector := &countsUpstreamCollector{fetch: func(context.Context, countsSourceSpec) countsAttempt {
		t.Fatal("unready fetch")
		return countsAttempt{}
	}}
	if got := collector.collect(context.Background(), source); got.Class != countsNotReady || got.CountsCalls != 0 {
		t.Fatal(got)
	}
}

type countsTestClosingBody struct {
	io.Reader
	closed *atomic.Int32
	once   sync.Once
}

func (b *countsTestClosingBody) Close() error { b.once.Do(func() { b.closed.Add(1) }); return nil }

func TestMediaCountsAttemptUsesUnifiedTransportAndSuppressesRecovery(t *testing.T) {
	for _, status := range []int{200, 204, 302, 401, 403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := countsTestClient("a")
			client.Config.SpoofClient = "hills"
			var closed, transportCalls, recovery atomic.Int32
			recoveryCalled := make(chan struct{}, 1)
			var requestProblem string
			transport := countsTestTransportFunc(func(r *http.Request) (*http.Response, error) {
				transportCalls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/a/Items/Counts" || r.URL.Query().Get("UserId") != "real-a" ||
					r.URL.Query().Has("IsFavorite") || r.Header.Get("X-Emby-Token") != "fixture-upstream-token" ||
					r.Header.Get("User-Agent") != spoofProfiles["hills"]["User-Agent"] {
					requestProblem = "identity/account/path mismatch"
				}
				body := `{"MovieCount":3,"SeriesCount":4,"EpisodeCount":5}`
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: &countsTestClosingBody{Reader: strings.NewReader(body), closed: &closed}, Request: r}, nil
			})
			client.httpClient = &http.Client{Transport: transport}
			client.onAuthError = func(*UpstreamClient) { recovery.Add(1); recoveryCalled <- struct{}{} }
			source := prepareCountsSource(client, nil, "", [32]byte{})
			got := requestCountsAttempt(context.Background(), source, false, time.Now)
			if status == 401 || status == 403 {
				select {
				case <-recoveryCalled:
					t.Fatal("Counts scheduled asynchronous auth recovery")
				case <-time.After(50 * time.Millisecond): // bounded negative assertion, not refresh time
				}
			}
			if requestProblem != "" {
				t.Fatal(requestProblem)
			}
			if !got.Sent || transportCalls.Load() != 1 || closed.Load() != 1 || recovery.Load() != 0 || got.Success != (status == 200) {
				t.Fatalf("result=%+v calls=%d closed=%d recovery=%d", got, transportCalls.Load(), closed.Load(), recovery.Load())
			}
		})
	}
}

func TestMediaCountsOfflineEvidenceAndUnreadyPassthrough(t *testing.T) {
	direct := countsTestClient("a")
	proxied := countsTestClient("a")
	proxied.Config.ProxyID = "fixture-proxy"
	for _, err := range []error{syscall.ECONNREFUSED, syscall.ENETUNREACH, syscall.EHOSTUNREACH, &net.DNSError{IsNotFound: true}} {
		if !countsDefinitelyUnreachable(err, direct) || countsDefinitelyUnreachable(err, proxied) {
			t.Fatal("proxy error manufactured offline")
		}
	}
	for _, err := range []error{context.DeadlineExceeded, errors.New("TLS failed"), &net.DNSError{IsTimeout: true}} {
		if countsDefinitelyUnreachable(err, direct) {
			t.Fatal("unknown error manufactured offline")
		}
	}
	direct.Config.SpoofClient = "passthrough"
	var calls atomic.Int32
	direct.httpClient = &http.Client{Transport: countsTestTransportFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected") })}
	source := prepareCountsSource(direct, nil, "", [32]byte{})
	got := requestCountsAttempt(context.Background(), source, false, time.Now)
	if source.Ready || got.Sent || calls.Load() != 0 || got.Class != countsNotReady {
		t.Fatal("invented passthrough identity")
	}
}

func TestMediaCountsAttemptProbeAndRedirectPolicy(t *testing.T) {
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop-redirect", true: "follow-redirect"}[follow], func(t *testing.T) {
			client := countsTestClient("a")
			client.Config.FollowRedirects = follow
			var calls, closed atomic.Int32
			transport := countsTestTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				headers := http.Header{}
				status, body := 200, `{"Id":"fixture-server"}`
				if r.URL.Path == "/a/System/Info/Public" {
					if r.URL.Query().Get("UserId") != "" || r.Header.Get("X-Emby-Token") != "fixture-upstream-token" {
						t.Error("probe used client filter or lost unified auth")
					}
					status = 302
					headers.Set("Location", "https://counts.invalid/a/probe-target")
				}
				return &http.Response{StatusCode: status, Header: headers, Request: r, Body: &countsTestClosingBody{Reader: strings.NewReader(body), closed: &closed}}, nil
			})
			client.httpClient = &http.Client{Transport: transport, CheckRedirect: redirectPolicy(follow)}
			source := prepareCountsSource(client, nil, "", [32]byte{})
			attempt := requestCountsAttempt(context.Background(), source, true, time.Now)
			expected := int32(1)
			if follow {
				expected = 2
			}
			if attempt.Success != follow || !attempt.Sent || calls.Load() != expected || closed.Load() != expected {
				t.Fatalf("attempt=%+v calls=%d closed=%d", attempt, calls.Load(), closed.Load())
			}
		})
	}
}

func TestMediaCountsAttemptTimeoutAndStaleSourceNoTransport(t *testing.T) {
	client := countsTestClient("a")
	client.timeouts.API = 5
	var calls atomic.Int32
	client.httpClient = &http.Client{Transport: countsTestTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	source := prepareCountsSource(client, nil, "", [32]byte{})
	attempt := requestCountsAttempt(context.Background(), source, false, time.Now)
	if !attempt.Sent || attempt.Success || attempt.Class != countsCanceled || calls.Load() != 1 {
		t.Fatal("API deadline not propagated to transport")
	}
	client.mu.Lock()
	client.AccessToken = "changed-token"
	client.mu.Unlock()
	attempt = requestCountsAttempt(context.Background(), source, false, time.Now)
	if attempt.Sent || attempt.Class != countsChanged || calls.Load() != 1 {
		t.Fatal("stale source started transport")
	}
}
