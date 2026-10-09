package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sampleSourceClient        = "client"
	sampleSourceHydrate       = "hydrate"
	sampleSourceParentHydrate = "parent_hydrate"
	sampleSourcePlayback      = "playback"
	sampleSourceFallback      = "fallback"
	sampleSourceMediaCounts   = "media_counts"
	sampleSourceHealthCheck   = "health_check"
	sampleSourceValidation    = "sample_validation"
	sampleSourceBackground    = "background"
)

type sampleTraceContextKey struct{}
type sampleSourceContextKey struct{}

func withSampleTrace(ctx context.Context, traceID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sampleTraceContextKey{}, traceID)
}

func sampleTraceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	traceID, _ := ctx.Value(sampleTraceContextKey{}).(string)
	return traceID
}

func withSampleSource(ctx context.Context, source string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sampleSourceContextKey{}, source)
}

func sampleSourceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	source, _ := ctx.Value(sampleSourceContextKey{}).(string)
	return source
}

type SampleCollector struct {
	enabled atomic.Bool

	mu sync.Mutex

	dataDir   string
	file      *os.File
	filePath  string
	sessionID string
	label      string
	targetUser string
	startedAt  time.Time
	events    int64
	dropped   int64

	inboundActive  int
	inboundPeak    int
	outboundActive int
	outboundPeak   int

	outboundActiveByUpstream map[string]int
	outboundPeakByUpstream   map[string]int
}

type sampleEvent struct {
	Timestamp           string         `json:"timestamp"`
	Event               string         `json:"event"`
	SessionID           string         `json:"session_id"`
	TraceID             string         `json:"trace_id,omitempty"`
	Source              string         `json:"source,omitempty"`
	User                string         `json:"user,omitempty"`
	Upstream            string         `json:"upstream,omitempty"`
	Method              string         `json:"method,omitempty"`
	Path                string         `json:"path,omitempty"`
	Status              int            `json:"status,omitempty"`
	DurationMS          int64          `json:"duration_ms,omitempty"`
	ResponseBytes       int64          `json:"response_bytes,omitempty"`
	ReturnedItems       *int           `json:"returned_items,omitempty"`
	Concurrency         int            `json:"concurrency,omitempty"`
	UpstreamConcurrency int            `json:"upstream_concurrency,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	Query               map[string]any `json:"query,omitempty"`
	Stats               map[string]int `json:"stats,omitempty"`
	ErrorClass          string         `json:"error_class,omitempty"`
	Label               string         `json:"label,omitempty"`
}

type sampleStatus struct {
	Active                 bool           `json:"active"`
	SessionID              string         `json:"sessionId,omitempty"`
	Label                  string         `json:"label,omitempty"`
	TargetUser             string         `json:"targetUser,omitempty"`
	StartedAt              string         `json:"startedAt,omitempty"`
	File                   string         `json:"file,omitempty"`
	Events                 int64          `json:"events"`
	Dropped                int64          `json:"dropped"`
	InboundActive          int            `json:"inboundActive"`
	InboundPeak            int            `json:"inboundPeak"`
	OutboundActive         int            `json:"outboundActive"`
	OutboundPeak           int            `json:"outboundPeak"`
	OutboundPeakByUpstream map[string]int `json:"outboundPeakByUpstream,omitempty"`
}

func newSampleCollector(dataDir string) *SampleCollector {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = defaultDataDir()
	}
	return &SampleCollector{
		dataDir:                  dataDir,
		outboundActiveByUpstream: map[string]int{},
		outboundPeakByUpstream:   map[string]int{},
	}
}

func (c *SampleCollector) Enabled() bool {
	return c != nil && c.enabled.Load()
}

func sanitizeSampleLabel(label string) string {
	label = strings.TrimSpace(label)
	runes := []rune(label)
	if len(runes) > 64 {
		label = string(runes[:64])
	}
	return label
}

func (c *SampleCollector) Start(label string, targetUsers ...string) (sampleStatus, error) {
	if c == nil {
		return sampleStatus{}, errors.New("sample collector unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enabled.Load() {
		return c.statusLocked(), errors.New("sample capture already active")
	}
	if err := os.MkdirAll(filepath.Join(c.dataDir, "samples"), 0o700); err != nil {
		return sampleStatus{}, err
	}
	sessionID := randomHex(8)
	now := time.Now()
	name := "sample-" + now.UTC().Format("20060102T150405.000000000Z") + "-" + sessionID + ".jsonl"
	path := filepath.Join(c.dataDir, "samples", name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return sampleStatus{}, err
	}
	if c.file != nil {
		_ = c.file.Close()
	}
	c.file = file
	c.filePath = path
	c.sessionID = sessionID
	c.label = sanitizeSampleLabel(label)
	c.targetUser = ""
	if len(targetUsers) > 0 { c.targetUser = sanitizeSampleLabel(targetUsers[0]) }
	c.startedAt = now
	c.events = 0
	c.dropped = 0
	c.inboundActive = 0
	c.inboundPeak = 0
	c.outboundActive = 0
	c.outboundPeak = 0
	c.outboundActiveByUpstream = map[string]int{}
	c.outboundPeakByUpstream = map[string]int{}
	c.enabled.Store(true)
	c.writeEventLocked(sampleEvent{
		Timestamp: now.UTC().Format(time.RFC3339Nano),
		Event:     "session_start",
		SessionID: sessionID,
		Label:     c.label,
		User:      c.targetUser,
	})
	return c.statusLocked(), nil
}

func (c *SampleCollector) Stop() sampleStatus {
	if c == nil {
		return sampleStatus{}
	}
	c.enabled.Store(false)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file != nil {
		c.writeEventLocked(sampleEvent{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Event:     "session_stop",
			SessionID: c.sessionID,
			Label:     c.label,
		})
		_ = c.file.Sync()
		_ = c.file.Close()
		c.file = nil
	}
	c.inboundActive = 0
	c.outboundActive = 0
	c.outboundActiveByUpstream = map[string]int{}
	return c.statusLocked()
}

func (c *SampleCollector) Close() {
	if c == nil {
		return
	}
	c.Stop()
}

func (c *SampleCollector) Status() sampleStatus {
	if c == nil {
		return sampleStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *SampleCollector) statusLocked() sampleStatus {
	peaks := make(map[string]int, len(c.outboundPeakByUpstream))
	for name, value := range c.outboundPeakByUpstream {
		peaks[name] = value
	}
	started := ""
	if !c.startedAt.IsZero() {
		started = c.startedAt.UTC().Format(time.RFC3339Nano)
	}
	file := ""
	if c.filePath != "" {
		file = filepath.Base(c.filePath)
	}
	return sampleStatus{
		Active:                 c.enabled.Load(),
		SessionID:              c.sessionID,
		Label:                  c.label,
		TargetUser:             c.targetUser,
		StartedAt:              started,
		File:                   file,
		Events:                 c.events,
		Dropped:                c.dropped,
		InboundActive:          c.inboundActive,
		InboundPeak:            c.inboundPeak,
		OutboundActive:         c.outboundActive,
		OutboundPeak:           c.outboundPeak,
		OutboundPeakByUpstream: peaks,
	}
}

func (c *SampleCollector) writeEventLocked(event sampleEvent) {
	if c.file == nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		c.dropped++
		return
	}
	data = append(data, '\n')
	if _, err := c.file.Write(data); err != nil {
		c.dropped++
		return
	}
	c.events++
}

func (c *SampleCollector) record(event sampleEvent) {
	if c == nil || !c.enabled.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled.Load() || c.file == nil || event.SessionID != c.sessionID {
		return
	}
	c.writeEventLocked(event)
}

func (c *SampleCollector) ExportPath() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.filePath == "" {
		return "", false
	}
	if c.file != nil {
		_ = c.file.Sync()
	}
	if _, err := os.Stat(c.filePath); err != nil {
		return "", false
	}
	return c.filePath, true
}

type sampleInboundSpan struct {
	collector   *SampleCollector
	sessionID   string
	traceID     string
	user        string
	method      string
	path        string
	query       map[string]any
	started     time.Time
	concurrency int
}

func (c *SampleCollector) BeginInbound(traceID, user, method, path string, query url.Values) *sampleInboundSpan {
	if c == nil || !c.enabled.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled.Load() || c.file == nil || (c.targetUser != "" && user != c.targetUser) {
		return nil
	}
	c.inboundActive++
	if c.inboundActive > c.inboundPeak {
		c.inboundPeak = c.inboundActive
	}
	return &sampleInboundSpan{
		collector: c, sessionID: c.sessionID, traceID: traceID, user: user,
		method: method, path: samplePathClass(path), query: sampleQuerySummary(query),
		started: time.Now(), concurrency: c.inboundActive,
	}
}

func (s *sampleInboundSpan) Finish(status int, responseBytes int64, routePattern string) {
	if s == nil || s.collector == nil {
		return
	}
	c := s.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.sessionID != c.sessionID {
		return
	}
	if c.inboundActive > 0 {
		c.inboundActive--
	}
	if !c.enabled.Load() || c.file == nil {
		return
	}
	path := s.path
	routePattern = strings.TrimSpace(routePattern)
	if routePattern != "" && routePattern != "/" {
		if _, rest, ok := strings.Cut(routePattern, " "); ok { routePattern = rest }
		path = routePattern
	}
	c.writeEventLocked(sampleEvent{
		Timestamp:     s.started.UTC().Format(time.RFC3339Nano),
		Event:         "inbound",
		SessionID:     s.sessionID,
		TraceID:       s.traceID,
		Source:        sampleSourceClient,
		User:          s.user,
		Method:        s.method,
		Path:          path,
		Status:        status,
		DurationMS:    time.Since(s.started).Milliseconds(),
		ResponseBytes: responseBytes,
		Concurrency:   s.concurrency,
		Query:         s.query,
	})
}

type sampleOutboundSpan struct {
	collector            *SampleCollector
	sessionID            string
	traceID              string
	source               string
	user                 string
	upstream             string
	method               string
	path                 string
	query                map[string]any
	stream               bool
	started              time.Time
	concurrency          int
	upstreamConcurrency  int
	returnedItems        *int
	once                 sync.Once
}

func (c *SampleCollector) BeginOutbound(traceID, source, user, upstream, method, path string, query url.Values, stream bool) *sampleOutboundSpan {
	if c == nil || !c.enabled.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled.Load() || c.file == nil || (c.targetUser != "" && user != "" && user != c.targetUser) {
		return nil
	}
	c.outboundActive++
	if c.outboundActive > c.outboundPeak {
		c.outboundPeak = c.outboundActive
	}
	c.outboundActiveByUpstream[upstream]++
	if c.outboundActiveByUpstream[upstream] > c.outboundPeakByUpstream[upstream] {
		c.outboundPeakByUpstream[upstream] = c.outboundActiveByUpstream[upstream]
	}
	return &sampleOutboundSpan{
		collector: c, sessionID: c.sessionID, traceID: traceID, source: source,
		user: user, upstream: upstream, method: method, path: samplePathClass(path),
		query: sampleQuerySummary(query), stream: stream, started: time.Now(),
		concurrency: c.outboundActive, upstreamConcurrency: c.outboundActiveByUpstream[upstream],
	}
}

func (s *sampleOutboundSpan) SetReturnedItems(count int) {
	if s == nil || count < 0 {
		return
	}
	value := count
	s.returnedItems = &value
}

func (s *sampleOutboundSpan) Finish(status int, responseBytes int64, err error) {
	if s == nil || s.collector == nil {
		return
	}
	s.once.Do(func() {
		c := s.collector
		c.mu.Lock()
		defer c.mu.Unlock()
		if s.sessionID != c.sessionID {
			return
		}
		if c.outboundActive > 0 {
			c.outboundActive--
		}
		if c.outboundActiveByUpstream[s.upstream] > 0 {
			c.outboundActiveByUpstream[s.upstream]--
		}
		if !c.enabled.Load() || c.file == nil {
			return
		}
		c.writeEventLocked(sampleEvent{
			Timestamp:           s.started.UTC().Format(time.RFC3339Nano),
			Event:               "outbound",
			SessionID:           s.sessionID,
			TraceID:             s.traceID,
			Source:              s.source,
			User:                s.user,
			Upstream:            s.upstream,
			Method:              s.method,
			Path:                s.path,
			Status:              status,
			DurationMS:          time.Since(s.started).Milliseconds(),
			ResponseBytes:       responseBytes,
			ReturnedItems:       s.returnedItems,
			Concurrency:         s.concurrency,
			UpstreamConcurrency: s.upstreamConcurrency,
			Stream:              s.stream,
			Query:               s.query,
			ErrorClass:          sampleErrorClass(err),
		})
	})
}

type sampleBodyCapture struct {
	io.ReadCloser
	span     *sampleOutboundSpan
	status   int
	bytes    int64
	expected int64
}

func (b *sampleBodyCapture) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	return n, err
}

func (b *sampleBodyCapture) Close() error {
	err := b.ReadCloser.Close()
	if b.span != nil {
		bytes := b.bytes
		if b.expected >= 0 { bytes = b.expected }
		b.span.Finish(b.status, bytes, nil)
	}
	return err
}

func setSampleReturnedItems(resp *http.Response, payload any) {
	if resp == nil || resp.Body == nil {
		return
	}
	body, ok := resp.Body.(*sampleBodyCapture)
	if !ok || body.span == nil {
		return
	}
	if count := sampleReturnedItemCount(payload); count >= 0 {
		body.span.SetReturnedItems(count)
	}
}

func sampleReturnedItemCount(payload any) int {
	switch typed := payload.(type) {
	case []any:
		return len(typed)
	case map[string]any:
		for _, key := range []string{"Items", "SearchHints"} {
			if rows, ok := typed[key].([]any); ok {
				return len(rows)
			}
		}
	}
	return -1
}

func sampleErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "transport"
	}
}

func sampleOutboundSource(ctx context.Context, reqCtx *RequestContext, path string) string {
	if isCountsRequestContext(ctx) {
		return sampleSourceMediaCounts
	}
	if source := sampleSourceFromContext(ctx); source != "" {
		return source
	}
	if sampleTraceFromContext(ctx) != "" || (reqCtx != nil && reqCtx.TraceID != "") {
		return sampleSourceClient
	}
	if path == "/System/Info/Public" || isUpstreamLoginPath(path) {
		return sampleSourceHealthCheck
	}
	return sampleSourceBackground
}

func sampleOutboundTrace(ctx context.Context, reqCtx *RequestContext) string {
	if reqCtx != nil && reqCtx.TraceID != "" {
		return reqCtx.TraceID
	}
	return sampleTraceFromContext(ctx)
}

func sampleProxyUsername(reqCtx *RequestContext) string {
	if reqCtx == nil || reqCtx.ProxyUser == nil {
		return ""
	}
	return reqCtx.ProxyUser.Username
}

func samplePathClass(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 {
		return "/"
	}
	if len(segments) > 0 && strings.EqualFold(segments[0], "emby") {
		segments = segments[1:]
	}
	for i := range segments {
		if i == 0 {
			continue
		}
		prev := strings.ToLower(segments[i-1])
		current := strings.ToLower(segments[i])
		switch prev {
		case "users":
			segments[i] = "{userId}"
		case "shows":
			if current != "nextup" {
				segments[i] = "{seriesId}"
			}
		case "videos", "audio", "playingitems", "favoriteitems", "playeditems":
			segments[i] = "{itemId}"
		case "items":
			switch current {
			case "counts", "latest", "resume":
			default:
				segments[i] = "{itemId}"
			}
		default:
			if sampleOpaqueHexID(segments[i]) {
				segments[i] = "{id}"
			}
		}
	}
	return "/" + strings.Join(segments, "/")
}

func sampleOpaqueHexID(value string) bool {
	if len(value) < 16 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func sampleQueryValue(values url.Values, wanted string) string {
	for key, items := range values {
		if strings.EqualFold(key, wanted) && len(items) > 0 {
			return items[0]
		}
	}
	return ""
}

func sampleQueryPresent(values url.Values, wanted string) bool {
	for key, items := range values {
		if strings.EqualFold(key, wanted) {
			for _, value := range items {
				if strings.TrimSpace(value) != "" {
					return true
				}
			}
		}
	}
	return false
}

func sampleQuerySummary(values url.Values) map[string]any {
	if len(values) == 0 {
		return nil
	}
	result := map[string]any{}
	for _, key := range []string{"Limit", "StartIndex"} {
		if raw := sampleQueryValue(values, key); raw != "" {
			if value, err := strconv.Atoi(raw); err == nil {
				result[strings.ToLower(key)] = value
			}
		}
	}
	for _, key := range []string{"Fields", "IncludeItemTypes", "SortBy", "SortOrder", "Filters", "Recursive"} {
		if raw := sampleQueryValue(values, key); raw != "" {
			if len(raw) > 512 {
				raw = raw[:512]
			}
			result[strings.ToLower(key)] = raw
		}
	}
	if term := sampleQueryValue(values, "SearchTerm"); term != "" {
		result["search_term_present"] = true
		result["search_term_length"] = len([]rune(term))
	}
	if raw := sampleQueryValue(values, "Ids"); raw != "" {
		count := 0
		for _, id := range strings.Split(raw, ",") {
			if strings.TrimSpace(id) != "" {
				count++
			}
		}
		result["ids_count"] = count
	}
	for _, key := range []string{"ParentId", "SeriesId", "SeasonId", "MediaSourceId", "PlaySessionId", "SessionId", "UserId"} {
		if sampleQueryPresent(values, key) {
			result[strings.ToLower(key)+"_present"] = true
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (c *SampleCollector) RecordMergeQuality(reqCtx *RequestContext, upstream string, stats map[string]int) {
	if c == nil || !c.enabled.Load() || len(stats) == 0 {
		return
	}
	user := sampleProxyUsername(reqCtx)
	c.mu.Lock()
	targetUser := c.targetUser
	c.mu.Unlock()
	if targetUser != "" && user != targetUser { return }
	session := c.Status().SessionID
	if session == "" {
		return
	}
	c.record(sampleEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Event:     "merge_quality",
		SessionID: session,
		TraceID:   func() string { if reqCtx != nil { return reqCtx.TraceID }; return "" }(),
		Source:    sampleSourceClient,
		User:      user,
		Upstream:  upstream,
		Stats:     stats,
	})
}

func sampleMergeQuality(items []map[string]any) map[string]int {
	stats := map[string]int{"encountered_items": len(items)}
	for _, item := range items {
		if kind, _ := item["Type"].(string); strings.TrimSpace(kind) != "" {
			stats["type_present"]++
			if kind == "Movie" || kind == "Episode" {
				stats["movie_episode_items"]++
			}
		}
		providers, _ := item["ProviderIds"].(map[string]any)
		anyProvider := false
		for _, provider := range mergeProviders {
			if raw, ok := providers[provider].(string); ok && strings.TrimSpace(raw) != "" {
				stats["provider_"+strings.ToLower(provider)]++
				anyProvider = true
			}
		}
		if anyProvider {
			stats["provider_any"]++
		}
		if sources, ok := item["MediaSources"].([]any); ok && len(sources) > 0 {
			stats["media_sources_present"]++
		}
	}
	return stats
}

func (a *App) handleAdminSampleStart(w http.ResponseWriter, r *http.Request) {
	if a.SampleCollector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "sample collector unavailable"})
		return
	}
	var body struct {
		Label string `json:"label"`
		User  string `json:"user"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSONBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid request body"})
			return
		}
	}
	body.User = strings.TrimSpace(body.User)
	if body.User != "" {
		if a.UserStore == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sample target user is unavailable"})
			return
		}
		user := a.UserStore.GetByUsername(body.User)
		if user == nil || !user.Enabled {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sample target user not found or disabled"})
			return
		}
		body.User = user.Username
	}
	status, err := a.SampleCollector.Start(body.Label, body.User)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "status": a.SampleCollector.Status()})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *App) handleAdminSampleStop(w http.ResponseWriter, r *http.Request) {
	if a.SampleCollector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "sample collector unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, a.SampleCollector.Stop())
}

func (a *App) handleAdminSampleStatus(w http.ResponseWriter, r *http.Request) {
	if a.SampleCollector == nil {
		writeJSON(w, http.StatusOK, sampleStatus{})
		return
	}
	writeJSON(w, http.StatusOK, a.SampleCollector.Status())
}

func (a *App) handleAdminSampleExport(w http.ResponseWriter, r *http.Request) {
	if a.SampleCollector == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "sample capture not found"})
		return
	}
	path, ok := a.SampleCollector.ExportPath()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "sample capture not found"})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(path)+"\"")
	http.ServeFile(w, r, path)
}
