package domain

import (
	"testing"
	"time"
)

func rebuildFixture(now time.Time) *State {
	return &State{
		ActiveLeaseByNode: map[string]string{},
		Nodes: map[string]ResolverNode{
			"live":   {ID: "live", State: NodeReady},
			"ended":  {ID: "ended", State: NodeLeased},
			"due":    {ID: "due", State: NodeLeased},
			"double": {ID: "double", State: NodeReady},
		},
		Leases: map[string]NodeLease{
			// A claim still inside its deadline keeps the node isolated.
			"l-live": {ID: "l-live", NodeID: "live", State: LeaseClaimed,
				ClaimedAt: now, ExpiresAt: now.Add(time.Minute)},
			// Reported and released leases never block reuse.
			"l-reported": {ID: "l-reported", NodeID: "ended", State: LeaseReported,
				ClaimedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
				FinishedAt: timePointer(now.Add(-time.Minute))},
			"l-released": {ID: "l-released", NodeID: "ended", State: LeaseReleased,
				ClaimedAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(-2 * time.Minute),
				FinishedAt: timePointer(now.Add(-2 * time.Minute))},
			// A claim that crossed its deadline before reload expires.
			"l-due": {ID: "l-due", NodeID: "due", State: LeaseClaimed,
				ClaimedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute)},
			// An older claim still live must keep the node isolated even though a
			// newer lease for the same node has already finished.
			"l-double-claimed": {ID: "l-double-claimed", NodeID: "double", State: LeaseClaimed,
				ClaimedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
			"l-double-ended": {ID: "l-double-ended", NodeID: "double", State: LeaseReported,
				ClaimedAt: now.Add(-30 * time.Second), ExpiresAt: now.Add(30 * time.Second),
				FinishedAt: timePointer(now)},
		},
	}
}

func TestRebuildLeaseOccupancy(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	state := rebuildFixture(now)

	RebuildLeaseOccupancy(state, now)

	for nodeID, want := range map[string]bool{
		"live":   true,
		"ended":  false,
		"due":    false,
		"double": true,
	} {
		if got := NodeLeaseOccupied(state, nodeID); got != want {
			t.Errorf("NodeLeaseOccupied(%q) = %v, want %v", nodeID, got, want)
		}
	}
	if state.ActiveLeaseByNode["live"] != "l-live" {
		t.Errorf("live node must point at its live lease, got %q", state.ActiveLeaseByNode["live"])
	}
	if state.ActiveLeaseByNode["double"] != "l-double-claimed" {
		t.Errorf("older live claim must survive a newer ended lease, got %q", state.ActiveLeaseByNode["double"])
	}
	if state.Leases["l-due"].State != LeaseExpired || state.Leases["l-due"].FinishedAt == nil {
		t.Errorf("due claim must be finalized as expired, got %+v", state.Leases["l-due"])
	}
	if state.Nodes["live"].State != NodeLeased {
		t.Errorf("live node must reconcile to leased, got %s", state.Nodes["live"].State)
	}
	for _, nodeID := range []string{"ended", "due"} {
		if state.Nodes[nodeID].State != NodeReady {
			t.Errorf("%s node must reconcile to ready, got %s", nodeID, state.Nodes[nodeID].State)
		}
	}
	if ActiveLeaseCount(state) != 2 {
		t.Errorf("ActiveLeaseCount = %d, want 2", ActiveLeaseCount(state))
	}

	// Rebuilding again must converge: ended leases never come back, live claims stay.
	RebuildLeaseOccupancy(state, now)
	if ActiveLeaseCount(state) != 2 {
		t.Errorf("second rebuild must be stable, active count = %d", ActiveLeaseCount(state))
	}
}

func TestRecordLeaseFinished(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	state := rebuildFixture(now)
	state.ActiveLeaseByNode = map[string]string{"live": "l-live", "other": "l-other"}

	RecordLeaseFinished(state, state.Leases["l-live"])
	if NodeLeaseOccupied(state, "live") {
		t.Fatalf("finishing the active lease must free the node")
	}
	if !NodeLeaseOccupied(state, "other") {
		t.Fatalf("finishing one lease must not touch other nodes")
	}

	// A stale finish for a lease that no longer owns the node must not release
	// the current claim.
	state.ActiveLeaseByNode["live"] = "l-new"
	RecordLeaseFinished(state, state.Leases["l-live"])
	if state.ActiveLeaseByNode["live"] != "l-new" {
		t.Fatalf("stale finish must not clear a newer claim")
	}
}
