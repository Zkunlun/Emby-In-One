package backend

// upstreamAuthSnapshot is one consistent read of an upstream's authentication
// state. UserID and AccessToken are written together by setOnline, so reading
// them separately can pair the user ID of one login with the token of another.
// Every outbound request that needs both must take one snapshot instead.
type upstreamAuthSnapshot struct {
	UserID      string
	AccessToken string
}

// authSnapshot reads UserID and AccessToken under a single RLock.
func (c *UpstreamClient) authSnapshot() upstreamAuthSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return upstreamAuthSnapshot{UserID: c.UserID, AccessToken: c.AccessToken}
}

// clientUserID returns the upstream's real user ID for single-field reads in
// handlers. Code that needs the user ID and the token together must use
// authSnapshot instead of calling this twice.
func (c *UpstreamClient) clientUserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.UserID
}

// serverIndexValue returns the upstream's configured index. ServerIndex is set
// once at construction and never mutated, so no lock is needed.
func (c *UpstreamClient) serverIndexValue() int {
	return c.ServerIndex
}

// This value stays inside process memory and is never logged or serialized.
// A carried-over token cannot make new credentials statistics-ready before the
// new configuration has actually authenticated.
type countsLoginBinding struct {
	baseURL  string
	username string
	password string
	apiKey   string
}

func (c *UpstreamClient) configuredCountsLoginBinding() countsLoginBinding {
	return countsLoginBinding{c.BaseURL, c.Config.Username, c.Config.Password, c.Config.APIKey}
}

type countsAuthState struct {
	Auth           upstreamAuthSnapshot
	Observation    uint64
	Online         bool
	Retired        bool
	LoginPending   bool
	AccountCurrent bool
}

// Login start and outcome publications are ordering evidence for statistics;
// no statistics request is allowed to publish an older offline observation
// over a later login, even when the token happens to be identical.
func (c *UpstreamClient) countsAuthState() countsAuthState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return countsAuthState{
		Auth:        upstreamAuthSnapshot{UserID: c.UserID, AccessToken: c.AccessToken},
		Observation: c.apiObservation, Online: c.Online,
		Retired: c.retired, LoginPending: c.apiLoginAttempts != 0,
		AccountCurrent: c.countsLoginConfig == c.configuredCountsLoginBinding(),
	}
}

func (c *UpstreamClient) beginAPIStateObservation() bool {
	defer c.notifyCountsState()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retired {
		return false
	}
	c.apiObservation++
	c.apiLoginAttempts++
	return true
}

func (c *UpstreamClient) endAPIStateObservation() {
	defer c.notifyCountsState()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apiLoginAttempts != 0 {
		c.apiLoginAttempts--
	}
	c.apiObservation++
}

// Requires c.mu held by countsSourceSpec.lockPublication.
func (c *UpstreamClient) publishCountsOfflineLocked(auth upstreamAuthSnapshot, observation uint64) bool {
	if c.retired || !c.Online || c.apiLoginAttempts != 0 || c.apiObservation != observation ||
		c.UserID != auth.UserID || c.AccessToken != auth.AccessToken {
		return false
	}
	c.apiObservation++
	c.Online = false
	c.LastError = "media counts API check: unreachable"
	return true
}
