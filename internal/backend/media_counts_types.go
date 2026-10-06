package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	countsMaximumDuration       = time.Duration(1<<63 - 1)
	countsPayloadLimit          = 256 * 1024
	countsProbeLimit            = 64 * 1024
	countsSourceMax       int64 = 2147483647
	countsJSONSafeMax     int64 = 9007199254740991
	countsRoundTimeout          = 90 * time.Second
	countsAPITimeout            = 30 * time.Second
	countsCheckTimeout          = 5 * time.Second
)

var (
	errInvalidCounts       = errors.New("invalid media counts payload")
	errCountsOutOfRange    = errors.New("media counts out of range")
	errCountsNotReady      = errors.New("media counts source not ready")
	errCountsSourceChanged = errors.New("media counts source changed")
)

// A successful zero is a complete value; absence is represented by HasSnapshot,
// never by a fabricated zero-valued mediaCounts.
type mediaCounts struct {
	MovieCount   int64 `json:"MovieCount"`
	SeriesCount  int64 `json:"SeriesCount"`
	EpisodeCount int64 `json:"EpisodeCount"`
}

func (v mediaCounts) validSource() bool {
	return v.MovieCount >= 0 && v.MovieCount <= countsSourceMax &&
		v.SeriesCount >= 0 && v.SeriesCount <= countsSourceMax &&
		v.EpisodeCount >= 0 && v.EpisodeCount <= countsSourceMax
}

func (v mediaCounts) add(other mediaCounts) (mediaCounts, error) {
	values := [3]int64{v.MovieCount, v.SeriesCount, v.EpisodeCount}
	addends := [3]int64{other.MovieCount, other.SeriesCount, other.EpisodeCount}
	for i := range values {
		if values[i] < 0 || addends[i] < 0 || values[i] > countsJSONSafeMax ||
			addends[i] > countsJSONSafeMax-values[i] {
			return mediaCounts{}, errCountsOutOfRange
		}
		values[i] += addends[i]
	}
	return mediaCounts{values[0], values[1], values[2]}, nil
}

// Read the decompressed response with an extra byte to distinguish an exact
// limit-sized body from a truncated larger body.
func readCountsBody(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return nil, errInvalidCounts
	}
	payload, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, errInvalidCounts
	}
	return payload, nil
}

// Decode a single top-level object while retaining duplicate-key detection for
// requested fields. Unknown official fields are syntactically decoded and ignored.
func countsObjectFields(payload []byte, wanted map[string]bool) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errInvalidCounts
	}
	fields := make(map[string]json.RawMessage, len(wanted))
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, errInvalidCounts
		}
		name, ok := key.(string)
		if !ok {
			return nil, errInvalidCounts
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, errInvalidCounts
		}
		if wanted[name] {
			if _, duplicate := fields[name]; duplicate {
				return nil, errInvalidCounts
			}
			fields[name] = raw
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, errInvalidCounts
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errInvalidCounts
	}
	for name := range wanted {
		if _, exists := fields[name]; !exists {
			return nil, errInvalidCounts
		}
	}
	return fields, nil
}

func decodeMediaCounts(payload []byte) (mediaCounts, error) {
	if len(payload) > countsPayloadLimit {
		return mediaCounts{}, errInvalidCounts
	}
	fields, err := countsObjectFields(payload, map[string]bool{
		"MovieCount": true, "SeriesCount": true, "EpisodeCount": true,
	})
	if err != nil {
		return mediaCounts{}, err
	}
	var values [3]int64
	for i, name := range []string{"MovieCount", "SeriesCount", "EpisodeCount"} {
		raw := bytes.TrimSpace(fields[name])
		if len(raw) == 0 {
			return mediaCounts{}, errInvalidCounts
		}
		for _, digit := range raw {
			if digit < '0' || digit > '9' {
				return mediaCounts{}, errInvalidCounts
			}
		}
		value, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || value > countsSourceMax {
			return mediaCounts{}, errCountsOutOfRange
		}
		values[i] = value
	}
	return mediaCounts{values[0], values[1], values[2]}, nil
}

func validCountsProbe(payload []byte) bool {
	if len(payload) > countsProbeLimit {
		return false
	}
	fields, err := countsObjectFields(payload, map[string]bool{"Id": true})
	if err != nil {
		return false
	}
	var id string
	return json.Unmarshal(fields["Id"], &id) == nil && strings.TrimSpace(id) != ""
}

type countsErrorClass string

const (
	countsOK        countsErrorClass = ""
	countsNotReady  countsErrorClass = "not-ready"
	countsChanged   countsErrorClass = "source-changed"
	countsCanceled  countsErrorClass = "canceled"
	countsTransport countsErrorClass = "transport"
	countsHTTP      countsErrorClass = "http"
	countsPayload   countsErrorClass = "payload"
	countsLimited   countsErrorClass = "limited"
	countsClosed    countsErrorClass = "closed"
)

type countsReachability uint8

const (
	countsReachabilityUnknown countsReachability = iota
	countsReachabilityOnline
	countsReachabilityOffline
)

type countsWait struct {
	Limited       bool
	HasServerWait bool
	NeedsFallback bool // A limited response in this round supplied no usable wait.
	Until         time.Time
}

func (wait countsWait) merge(other countsWait) countsWait {
	wait.Limited = wait.Limited || other.Limited
	wait.HasServerWait = wait.HasServerWait || other.HasServerWait
	wait.NeedsFallback = wait.NeedsFallback || other.NeedsFallback
	if other.Until.After(wait.Until) {
		wait.Until = other.Until
	}
	return wait
}

type countsAttempt struct {
	Value                 mediaCounts
	Success               bool
	Sent                  bool // Application transport attempt, not wire requests/packets.
	Status                int
	Class                 countsErrorClass
	Wait                  countsWait
	DefinitelyUnreachable bool
}

type countsRoundResult struct {
	Value        mediaCounts
	Success      bool
	Class        countsErrorClass
	Status       int
	CheckStatus  int
	Reachability countsReachability
	Wait         countsWait
	CountsCalls  int
	CheckCalls   int
}

type countsSnapshot struct {
	Value          mediaCounts
	SucceededAt    time.Time
	DataGeneration uint64
}
