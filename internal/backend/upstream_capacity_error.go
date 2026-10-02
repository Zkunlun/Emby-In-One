package backend

import (
	"errors"
	"fmt"
)

// UpstreamCapacityErrorCode identifies a capacity conflict without coupling the
// storage layer to HTTP response details.
type UpstreamCapacityErrorCode string

const (
	UpstreamCapacityFull          UpstreamCapacityErrorCode = "UPSTREAM_CAPACITY_FULL"
	UpstreamCapacityBelowAssigned UpstreamCapacityErrorCode = "UPSTREAM_CAPACITY_BELOW_ASSIGNED"
)

// UpstreamCapacityError describes a conflict between an upstream's configured
// capacity and the number of regular users explicitly assigned to it.
//
// Limit is the requested/effective maxConcurrent value. Assigned is the current
// number of explicit regular-user grants. ServerID is the stable upstream ID;
// callers that need a display name can resolve it from ConfigStore.
type UpstreamCapacityError struct {
	Code     UpstreamCapacityErrorCode
	ServerID string
	Limit    int
	Assigned int
}

func (e *UpstreamCapacityError) Error() string {
	if e == nil {
		return "upstream capacity conflict"
	}
	switch e.Code {
	case UpstreamCapacityFull:
		return fmt.Sprintf("upstream %s capacity full: assigned=%d limit=%d", e.ServerID, e.Assigned, e.Limit)
	case UpstreamCapacityBelowAssigned:
		return fmt.Sprintf("upstream %s capacity limit %d is below assigned users %d", e.ServerID, e.Limit, e.Assigned)
	default:
		return fmt.Sprintf("upstream %s capacity conflict: assigned=%d limit=%d", e.ServerID, e.Assigned, e.Limit)
	}
}

func newUpstreamCapacityFullError(serverID string, limit, assigned int) *UpstreamCapacityError {
	return &UpstreamCapacityError{
		Code:     UpstreamCapacityFull,
		ServerID: serverID,
		Limit:    limit,
		Assigned: assigned,
	}
}

func newUpstreamCapacityBelowAssignedError(serverID string, limit, assigned int) *UpstreamCapacityError {
	return &UpstreamCapacityError{
		Code:     UpstreamCapacityBelowAssigned,
		ServerID: serverID,
		Limit:    limit,
		Assigned: assigned,
	}
}

// asUpstreamCapacityError extracts a capacity conflict from an error chain so
// handlers can map domain conflicts to their public API contract without
// treating unrelated storage/configuration failures as capacity errors.
func asUpstreamCapacityError(err error) (*UpstreamCapacityError, bool) {
	var capacityErr *UpstreamCapacityError
	if errors.As(err, &capacityErr) {
		return capacityErr, true
	}
	return nil, false
}
