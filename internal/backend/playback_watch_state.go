package backend

import (
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// These values describe only local watch events. They do not replace the
// upstream request body or the playback lease's real device/session identity.
type playbackWatchEventKind uint8

const (
	playbackWatchStarted playbackWatchEventKind = iota
	playbackWatchProgress
	playbackWatchStopped
)

type playbackTickState uint8

const (
	playbackTicksMissing playbackTickState = iota
	playbackTicksNull
	playbackTicksValid
	playbackTicksInvalid
)

// Keep absence separate from a genuine zero position. Invalid values must not
// clear resume state or silently borrow another event's position.
type playbackTickValue struct {
	Ticks int64
	State playbackTickState
}

func validPlaybackTicks(ticks int64) playbackTickValue {
	if ticks < 0 {
		return playbackTickValue{State: playbackTicksInvalid}
	}
	return playbackTickValue{Ticks: ticks, State: playbackTicksValid}
}

func (v playbackTickValue) valid() bool {
	return v.State == playbackTicksValid && v.Ticks >= 0
}

func (v playbackTickValue) absent() bool {
	return v.State == playbackTicksMissing || v.State == playbackTicksNull
}

// Parse only the fields used for local watch state. The existing general JSON
// decoder and the body forwarded upstream are deliberately left unchanged.
func playbackTicksFromBody(body map[string]any, field string) playbackTickValue {
	raw, present := body[field]
	if !present {
		return playbackTickValue{State: playbackTicksMissing}
	}
	if raw == nil {
		return playbackTickValue{State: playbackTicksNull}
	}
	switch value := raw.(type) {
	case int64:
		return validPlaybackTicks(value)
	case int:
		return validPlaybackTicks(int64(value))
	case json.Number:
		if ticks, err := value.Int64(); err == nil {
			return validPlaybackTicks(ticks)
		}
	case float64:
		// json.Unmarshal produces float64. Reject the range where a rounded JSON
		// integer cannot be distinguished from its adjacent integer.
		const exactIntegerLimit = 1 << 53
		if !math.IsNaN(value) && !math.IsInf(value, 0) &&
			value >= 0 && value < exactIntegerLimit && math.Trunc(value) == value {
			return validPlaybackTicks(int64(value))
		}
	}
	return playbackTickValue{State: playbackTicksInvalid}
}

func playbackTicksFromQuery(query url.Values, field string) playbackTickValue {
	values, present := query[field]
	if !present {
		return playbackTickValue{State: playbackTicksMissing}
	}
	// Duplicates are ambiguous even when one value happens to look valid.
	if len(values) != 1 {
		return playbackTickValue{State: playbackTicksInvalid}
	}
	ticks, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		return playbackTickValue{State: playbackTicksInvalid}
	}
	return validPlaybackTicks(ticks)
}

type playbackWatchSource struct {
	ServerID       string
	OriginalItemID string
	MediaSourceID  string
}

func (source playbackWatchSource) valid() bool {
	return source.ServerID != "" && source.OriginalItemID != ""
}

type playbackWatchSession struct {
	ProxyUserID      string
	PlaybackDeviceID string
	PlaySessionID    string
	Source           playbackWatchSource
}

func (session playbackWatchSession) identified() bool {
	return session.ProxyUserID != "" && session.PlaybackDeviceID != "" &&
		session.PlaySessionID != "" && session.Source.valid() &&
		session.Source.MediaSourceID != ""
}

type playbackRuntimeCandidate struct {
	Source playbackWatchSource
	Ticks  int64
	// Set only when metadata proves a single available media source. A history
	// row with a positive runtime does not by itself prove this property.
	SingleMediaSource bool
}

func (candidate playbackRuntimeCandidate) matches(source playbackWatchSource) bool {
	if !source.valid() || !candidate.Source.valid() ||
		candidate.Source.ServerID != source.ServerID ||
		candidate.Source.OriginalItemID != source.OriginalItemID {
		return false
	}
	if source.MediaSourceID != "" {
		return candidate.Source.MediaSourceID == source.MediaSourceID
	}
	return candidate.SingleMediaSource
}

// Candidates are supplied in priority order after their cache/session origin
// has been checked. This helper never reads unqualified WatchStore.RuntimeTicks
// and never performs network I/O.
func resolvePlaybackRuntime(source playbackWatchSource, reported playbackTickValue, candidates ...playbackRuntimeCandidate) playbackTickValue {
	if !source.valid() {
		return playbackTickValue{State: playbackTicksMissing}
	}
	if reported.valid() && reported.Ticks > 0 {
		return reported
	}
	for _, candidate := range candidates {
		if candidate.Ticks > 0 && candidate.matches(source) {
			return validPlaybackTicks(candidate.Ticks)
		}
	}
	return playbackTickValue{State: playbackTicksMissing}
}

type playbackPositionCandidate struct {
	Session  playbackWatchSession
	Position playbackTickValue
}

// A terminal event with no position can reuse only a fully identified matching
// session. Zero and malformed positions are never replaced by cached progress.
func resolvePlaybackPosition(kind playbackWatchEventKind, reported playbackTickValue, session playbackWatchSession, candidate playbackPositionCandidate) playbackTickValue {
	if kind == playbackWatchStopped && reported.absent() && session.identified() &&
		candidate.Session == session && candidate.Position.valid() {
		return candidate.Position
	}
	return reported
}

type playbackWatchEvent struct {
	Kind     playbackWatchEventKind
	Source   playbackWatchSource
	Position playbackTickValue
	Runtime  playbackTickValue
	Failed   bool
	Live     bool
}

func playbackWatchEventFromBody(kind playbackWatchEventKind, body map[string]any) playbackWatchEvent {
	event := playbackWatchEvent{
		Kind:     kind,
		Position: playbackTicksFromBody(body, "PositionTicks"),
		Runtime:  playbackTicksFromBody(body, "RunTimeTicks"),
	}
	if raw, present := body["Failed"]; present && raw != nil {
		failed, ok := raw.(bool)
		event.Failed = !ok || failed
	}
	if raw, present := body["LiveStreamId"]; present && raw != nil {
		id, ok := raw.(string)
		event.Live = !ok || strings.TrimSpace(id) != ""
	}
	return event
}

func playbackWatchEventFromQuery(kind playbackWatchEventKind, query url.Values) playbackWatchEvent {
	return playbackWatchEvent{
		Kind:     kind,
		Position: playbackTicksFromQuery(query, "PositionTicks"),
		// The official legacy PlayingItems endpoints do not declare a runtime
		// query parameter. Duration must come from a qualified source instead.
		Live: len(query["LiveStreamId"]) > 1 || strings.TrimSpace(query.Get("LiveStreamId")) != "",
	}
}

func (event playbackWatchEvent) completionCandidate() bool {
	if (event.Kind != playbackWatchProgress && event.Kind != playbackWatchStopped) ||
		event.Failed || event.Live || !event.Source.valid() ||
		!event.Position.valid() || !event.Runtime.valid() ||
		event.Position.Ticks <= 0 || event.Runtime.Ticks <= 0 {
		return false
	}
	// runtime - floor(runtime/10) is ceil(90% of runtime), without multiplication,
	// float rounding, or overflow even at the int64 limit.
	return event.Position.Ticks >= event.Runtime.Ticks-event.Runtime.Ticks/10
}

func autoPlayedItemType(itemType string) bool {
	return itemType == "Movie" || itemType == "Episode"
}
