package backend

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Only internal statistics callers install this control. Normal playback/API
// calls retain their existing recovery callbacks and auth preparation.
type countsRequestKey struct{}
type countsRequestControl struct {
	Source             countsSourceSpec
	TransportAttempted *bool
}

func isCountsRequestContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(countsRequestKey{}).(countsRequestControl)
	return ok
}

func recordCountsTransportAttempt(ctx context.Context) {
	if ctx == nil {
		return
	}
	if control, ok := ctx.Value(countsRequestKey{}).(countsRequestControl); ok && control.TransportAttempted != nil {
		*control.TransportAttempted = true
	}
}

func countsAuthForRequest(ctx context.Context, client *UpstreamClient) (upstreamAuthSnapshot, bool, error) {
	if ctx == nil {
		return upstreamAuthSnapshot{}, false, nil
	}
	control, ok := ctx.Value(countsRequestKey{}).(countsRequestControl)
	if !ok {
		return upstreamAuthSnapshot{}, false, nil
	}
	if control.Source.Client != client || !control.Source.current() {
		return upstreamAuthSnapshot{}, true, errCountsSourceChanged
	}
	// URL, body and headers all use this same frozen account/token, even if a
	// concurrent login starts after the last current-source check.
	return control.Source.Auth, true, nil
}

type countsSourceSpec struct {
	Client          *UpstreamClient
	Identity        *ClientIdentityService
	Auth            upstreamAuthSnapshot
	Observation     uint64
	IdentityEpoch   uint64
	IdentityHeaders http.Header
	IdentitySource  string
	DataSignature   [32]byte
	WorkSignature   [32]byte
	signingKey      [32]byte
	Ready           bool
}

func countsSignature(key [32]byte, fields []string) [32]byte {
	encoded, _ := json.Marshal(fields) // []string cannot fail to marshal.
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(encoded)
	var signature [32]byte
	copy(signature[:], mac.Sum(nil))
	return signature
}

// Pure data fingerprint; callers supply the current account and resolved proxy.
// It takes no locks, captures no client identity, and never starts work.
func countsDataSignature(client *UpstreamClient, userID, proxyURL string, key [32]byte) [32]byte {
	cfg := client.Config
	return countsSignature(key, []string{
		client.ID, client.BaseURL, cfg.Username, cfg.Password, cfg.APIKey, userID,
		cfg.SpoofClient, cfg.CustomUserAgent, cfg.CustomClient, cfg.CustomClientVersion,
		cfg.CustomDeviceName, cfg.CustomDeviceId, cfg.ProxyID, proxyURL, strconv.FormatBool(cfg.FollowRedirects),
	})
}

// proxyURL is the resolved URL from the currently published Config snapshot,
// not merely ProxyID. The HMAC key is private to the cache and never persisted.
func prepareCountsSource(client *UpstreamClient, identity *ClientIdentityService, proxyURL string, key [32]byte) countsSourceSpec {
	source := countsSourceSpec{Client: client, Identity: identity, signingKey: key}
	if client == nil {
		return source
	}
	state := client.countsAuthState()
	source.Auth, source.Observation = state.Auth, state.Observation
	source.IdentityEpoch = identity.captureEpoch()
	source.IdentitySource, source.IdentityHeaders = client.resolveIdentityHeaders(nil, identity, nil)
	cfg := client.Config
	source.DataSignature = countsDataSignature(client, state.Auth.UserID, proxyURL, key)
	source.WorkSignature = source.workSignature(state)
	source.Ready = client.ID != "" && state.Online && !state.Retired && !state.LoginPending && state.AccountCurrent &&
		state.Auth.UserID != "" && state.Auth.AccessToken != "" &&
		(cfg.SpoofClient != "passthrough" || (source.IdentitySource != "infuse-fallback" &&
			hasPassthroughIdentity(source.IdentityHeaders)))
	return source
}

func (source countsSourceSpec) workSignature(state countsAuthState) [32]byte {
	headers, _ := json.Marshal(source.IdentityHeaders)
	return countsSignature(source.signingKey, []string{
		string(source.DataSignature[:]), state.Auth.AccessToken, strconv.FormatUint(state.Observation, 10),
		strconv.FormatUint(source.IdentityEpoch, 10), source.IdentitySource, string(headers),
		strconv.FormatBool(state.Online), strconv.FormatBool(state.Retired), strconv.FormatBool(state.LoginPending),
		strconv.FormatBool(state.AccountCurrent),
	})
}

func (source countsSourceSpec) current() bool {
	if !source.Ready || source.Client == nil {
		return false
	}
	state := source.Client.countsAuthState()
	if state.Retired || !state.Online || state.LoginPending || !state.AccountCurrent || state.Auth != source.Auth ||
		state.Observation != source.Observation || source.Identity.captureEpoch() != source.IdentityEpoch {
		return false
	}
	label, headers := source.Client.resolveIdentityHeaders(nil, source.Identity, nil)
	candidate := source
	candidate.IdentitySource, candidate.IdentityHeaders = label, headers
	return candidate.workSignature(state) == source.WorkSignature
}

// lockPublication follows lifecycle -> cache -> client -> identity. The client
// and identity locks remain held across snapshot/offline publication, so a login
// start or capture change cannot slip between validation and the cache write.
func (source countsSourceSpec) lockPublication() (func(), bool) {
	if !source.Ready || source.Client == nil {
		return nil, false
	}
	client := source.Client
	client.mu.Lock()
	identity := source.Identity
	if identity != nil {
		identity.mu.RLock()
	}
	release := func() {
		if identity != nil {
			identity.mu.RUnlock()
		}
		client.mu.Unlock()
	}
	state := countsAuthState{
		Auth:        upstreamAuthSnapshot{UserID: client.UserID, AccessToken: client.AccessToken},
		Observation: client.apiObservation, Online: client.Online,
		Retired: client.retired, LoginPending: client.apiLoginAttempts != 0,
		AccountCurrent: client.countsLoginConfig == client.configuredCountsLoginBinding(),
	}
	epoch := uint64(0)
	if identity != nil {
		epoch = identity.lifecycleEpoch
	}
	if state.Retired || !state.Online || state.LoginPending || !state.AccountCurrent || state.Auth != source.Auth ||
		state.Observation != source.Observation || epoch != source.IdentityEpoch {
		release()
		return nil, false
	}
	// Resolve only the background identity candidates while holding identity.mu.
	// Do not recursively call the locking public resolver.
	candidate := source
	if client.Config.SpoofClient == "passthrough" {
		candidate.IdentitySource = "infuse-fallback"
		candidate.IdentityHeaders = nil
		if identity != nil {
			if entry, ok := identity.lastSuccessByServer[client.serverKey]; ok && hasPassthroughIdentity(entry.headers) {
				candidate.IdentitySource = "last-success"
				candidate.IdentityHeaders = mergePassthroughHeaders(entry.headers)
			} else if identity.latestCaptured != nil && hasPassthroughIdentity(identity.latestCaptured.headers) {
				candidate.IdentitySource = "captured-latest"
				candidate.IdentityHeaders = mergePassthroughHeaders(identity.latestCaptured.headers)
			}
		}
	} else {
		candidate.IdentitySource, candidate.IdentityHeaders = client.resolveIdentityHeaders(nil, nil, nil)
	}
	if candidate.workSignature(state) != source.WorkSignature {
		release()
		return nil, false
	}
	return release, true
}

func countsOperationTimeout(milliseconds int, maximum time.Duration) time.Duration {
	// Compare before multiplication to avoid overflowing a duration.
	if milliseconds <= 0 || int64(milliseconds) >= maximum.Milliseconds() {
		return maximum
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func countsError(err error) countsErrorClass {
	if errors.Is(err, errCountsSourceChanged) {
		return countsChanged
	}
	if errors.Is(err, errCountsNotReady) {
		return countsNotReady
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return countsCanceled
	}
	if _, ok := asPreparationError(err); ok {
		return countsNotReady
	}
	return countsTransport
}

// The application only has a definitive upstream-offline observation for a
// direct API target. Proxy/DNS/TLS/timeout failures can otherwise describe an
// intermediary or an unknown state and must not manufacture offline zero.
func countsDefinitelyUnreachable(err error, client *UpstreamClient) bool {
	if err == nil || client == nil || client.Config.ProxyID != "" {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}

func countsRetryAfter(headers http.Header, status int, now time.Time) countsWait {
	wait := countsWait{Limited: status == http.StatusTooManyRequests}
	serverDate, dateErr := http.ParseTime(headers.Get("Date"))
	for _, raw := range headers.Values("Retry-After") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		allDigits := true
		for _, digit := range raw {
			if digit < '0' || digit > '9' {
				allDigits = false
				break
			}
		}
		var until time.Time
		if allDigits {
			seconds, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || seconds > uint64(countsMaximumDuration/time.Second) {
				until = now.Add(countsMaximumDuration)
			} else {
				until = now.Add(time.Duration(seconds) * time.Second)
			}
		} else {
			date, err := http.ParseTime(raw)
			if err != nil {
				continue
			}
			until = date
			if dateErr == nil {
				relative := now.Add(date.Sub(serverDate))
				if relative.After(until) {
					until = relative
				}
			}
		}
		minimum := now.Add(time.Minute)
		if until.Before(minimum) {
			until = minimum
		}
		wait.Limited, wait.HasServerWait = true, true
		if until.After(wait.Until) {
			wait.Until = until
		}
	}
	wait.NeedsFallback = status == http.StatusTooManyRequests && !wait.HasServerWait
	return wait
}

func requestCountsAttempt(ctx context.Context, source countsSourceSpec, probe bool, now func() time.Time) countsAttempt {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return countsAttempt{Class: countsCanceled}
	}
	if !source.Ready {
		return countsAttempt{Class: countsNotReady}
	}
	if !source.current() {
		return countsAttempt{Class: countsChanged}
	}
	path, limit, maximum, timeoutMS := "/Items/Counts", int64(countsPayloadLimit), countsAPITimeout, source.Client.timeouts.API
	params := url.Values{"UserId": {source.Auth.UserID}}
	if probe {
		path, limit, maximum, timeoutMS = "/System/Info/Public", countsProbeLimit, countsCheckTimeout, source.Client.timeouts.HealthCheck
		params = nil
	}
	attemptCtx, cancel := context.WithTimeout(ctx, countsOperationTimeout(timeoutMS, maximum))
	defer cancel()
	sent := false
	attemptCtx = context.WithValue(attemptCtx, countsRequestKey{}, countsRequestControl{Source: source, TransportAttempted: &sent})
	response, err := source.Client.doRequestForMode(attemptCtx, nil, http.MethodGet, path, params, nil,
		cloneHeader(source.IdentityHeaders), false, authModeNormal)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		class := countsError(err)
		return countsAttempt{Class: class, Sent: sent,
			DefinitelyUnreachable: countsDefinitelyUnreachable(err, source.Client)}
	}
	if response == nil {
		return countsAttempt{Class: countsTransport, Sent: sent}
	}
	attempt := countsAttempt{Sent: sent, Status: response.StatusCode}
	if response.StatusCode != http.StatusOK {
		attempt.Class = countsHTTP
		attempt.Wait = countsRetryAfter(response.Header, response.StatusCode, now())
		if attempt.Wait.Limited {
			attempt.Class = countsLimited
		}
		// Never decode/reflect an upstream error body or infer rate limits from it.
		return attempt
	}
	body, err := readCountsBody(response.Body, limit)
	if err != nil {
		attempt.Class = countsPayload
		if attemptCtx.Err() != nil {
			attempt.Class = countsCanceled
		}
		return attempt
	}
	if probe {
		attempt.Success = validCountsProbe(body)
	} else {
		attempt.Value, err = decodeMediaCounts(body)
		attempt.Success = err == nil
	}
	if !attempt.Success {
		attempt.Class = countsPayload
	}
	if attemptCtx.Err() != nil {
		attempt.Success, attempt.Class = false, countsCanceled
	}
	return attempt
}

// Injected clock/fetch/check functions support controlled Phase4 tests. They
// cannot be configured by HTTP requests and the production default remains the
// unified upstream path above.
type countsUpstreamCollector struct {
	now   func() time.Time
	fetch func(context.Context, countsSourceSpec) countsAttempt
	check func(context.Context, countsSourceSpec) countsAttempt
}

func newCountsUpstreamCollector(now func() time.Time) *countsUpstreamCollector {
	if now == nil {
		now = time.Now
	}
	return &countsUpstreamCollector{
		now: now,
		fetch: func(ctx context.Context, source countsSourceSpec) countsAttempt {
			return requestCountsAttempt(ctx, source, false, now)
		},
		check: func(ctx context.Context, source countsSourceSpec) countsAttempt {
			return requestCountsAttempt(ctx, source, true, now)
		},
	}
}

func countsRoundStopped(ctx context.Context, source countsSourceSpec) countsErrorClass {
	if ctx.Err() != nil {
		return countsCanceled
	}
	if !source.Ready {
		return countsNotReady
	}
	if !source.current() {
		return countsChanged
	}
	return countsOK
}

func (collector *countsUpstreamCollector) collect(ctx context.Context, source countsSourceSpec) countsRoundResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, countsRoundTimeout)
	defer cancel()
	result := countsRoundResult{Reachability: countsReachabilityUnknown}
	if stopped := countsRoundStopped(ctx, source); stopped != countsOK {
		result.Class = stopped
		return result
	}
	first := collector.fetch(ctx, source)
	if first.Sent {
		result.CountsCalls++
	}
	result.Class, result.Status, result.Wait = first.Class, first.Status, first.Wait
	if stopped := countsRoundStopped(ctx, source); stopped != countsOK {
		result.Class = stopped
		return result
	}
	if first.Success {
		result.Value, result.Success, result.Class = first.Value, true, countsOK
		return result
	}
	// Preparation/retirement never produced a Counts transport attempt.
	if !first.Sent || first.Class == countsChanged || first.Class == countsNotReady {
		return result
	}
	// A per-Counts timeout may leave time for the bounded check; only the round
	// cancellation above stops the entire chain.
	check := collector.check(ctx, source)
	if check.Sent {
		result.CheckCalls++
	}
	result.CheckStatus = check.Status
	result.Wait = result.Wait.merge(check.Wait)
	if stopped := countsRoundStopped(ctx, source); stopped != countsOK {
		result.Class = stopped
		return result
	}
	if check.DefinitelyUnreachable {
		result.Reachability = countsReachabilityOffline
		return result
	}
	if check.Success {
		result.Reachability = countsReachabilityOnline
	}
	if result.Wait.Limited {
		result.Class = countsLimited
		return result
	}
	if result.Reachability != countsReachabilityOnline {
		return result
	}
	second := collector.fetch(ctx, source)
	if second.Sent {
		result.CountsCalls++
	}
	result.Class, result.Status = second.Class, second.Status
	result.Wait = result.Wait.merge(second.Wait)
	if stopped := countsRoundStopped(ctx, source); stopped != countsOK {
		result.Class = stopped
		return result
	}
	if second.Success {
		result.Value, result.Success, result.Class = second.Value, true, countsOK
	}
	return result
}
