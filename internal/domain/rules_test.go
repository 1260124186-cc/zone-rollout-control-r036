package domain

import (
	"testing"
	"time"
)

func TestRecordLeaseFinishedRemovesNodeOccupancy(t *testing.T) {
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	lease := NodeLease{
		ID:        "lse_active",
		NodeID:    "nod_1",
		State:     LeaseClaimed,
		ExpiresAt: now.Add(time.Minute),
	}
	state := &State{ActiveLeaseByNode: map[string]string{"nod_1": lease.ID}}

	lease.State = LeaseReleased
	lease.FinishedAt = timePointer(now)
	RecordLeaseFinished(state, lease)

	if NodeLeaseOccupied(state, "nod_1") {
		t.Fatal("finished lease must release node occupancy")
	}
	if ActiveLeaseCount(state) != 0 {
		t.Fatalf("active lease count = %d, want 0", ActiveLeaseCount(state))
	}
}

func TestLeaseIsCurrentMatchesNodeOccupancy(t *testing.T) {
	state := &State{ActiveLeaseByNode: map[string]string{"nod_1": "lse_new"}}

	if !LeaseIsCurrent(state, NodeLease{ID: "lse_new", NodeID: "nod_1"}) {
		t.Fatal("matching lease and node must be current")
	}
	if LeaseIsCurrent(state, NodeLease{ID: "lse_old", NodeID: "nod_1"}) {
		t.Fatal("older duplicate claim must not be current")
	}
}

func TestRebuildLeaseOccupancyUsesOnlyUnexpiredClaimedLeases(t *testing.T) {
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	leases := []NodeLease{
		{ID: "lse_claimed", NodeID: "nod_claimed", State: LeaseClaimed, ExpiresAt: now.Add(time.Minute)},
		{ID: "lse_reported", NodeID: "nod_reported", State: LeaseReported, ExpiresAt: now.Add(time.Minute), FinishedAt: timePointer(now)},
		{ID: "lse_released", NodeID: "nod_released", State: LeaseReleased, ExpiresAt: now.Add(time.Minute), FinishedAt: timePointer(now)},
		{ID: "lse_expired", NodeID: "nod_expired", State: LeaseClaimed, ExpiresAt: now.Add(-time.Minute)},
	}
	state := &State{
		ActiveLeaseByNode: map[string]string{
			"nod_reported": "lse_reported",
			"nod_released": "lse_released",
		},
		Nodes: map[string]ResolverNode{
			"nod_claimed":  {ID: "nod_claimed", State: NodeReady},
			"nod_reported": {ID: "nod_reported", State: NodeLeased},
			"nod_released": {ID: "nod_released", State: NodeLeased},
			"nod_expired":  {ID: "nod_expired", State: NodeLeased},
		},
		Leases: make(map[string]NodeLease),
	}
	for _, lease := range leases {
		state.Leases[lease.ID] = lease
	}

	RebuildLeaseOccupancyAt(state, now)

	if !NodeLeaseOccupied(state, "nod_claimed") {
		t.Fatal("unexpired claimed lease must keep its node occupied")
	}
	for nodeID := range map[string]struct{}{"nod_reported": {}, "nod_released": {}, "nod_expired": {}} {
		if NodeLeaseOccupied(state, nodeID) {
			t.Fatalf("%s must not remain occupied after its lease ended", nodeID)
		}
	}
	if state.Nodes["nod_claimed"].State != NodeLeased {
		t.Fatalf("claimed node state = %s, want leased", state.Nodes["nod_claimed"].State)
	}
	for nodeID := range map[string]struct{}{"nod_reported": {}, "nod_released": {}, "nod_expired": {}} {
		if state.Nodes[nodeID].State != NodeReady {
			t.Fatalf("%s node state = %s, want ready", nodeID, state.Nodes[nodeID].State)
		}
	}
	if state.Leases["lse_expired"].State != LeaseExpired {
		t.Fatalf("expired-due lease state = %s, want expired", state.Leases["lse_expired"].State)
	}
}

func TestRebuildLeaseOccupancyResolvesDuplicateClaims(t *testing.T) {
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	older := NodeLease{ID: "lse_older", PlanID: "plan_1", NodeID: "nod_1", State: LeaseClaimed, ClaimedAt: now, ExpiresAt: now.Add(time.Minute)}
	newer := NodeLease{ID: "lse_newer", PlanID: "plan_2", NodeID: "nod_1", State: LeaseClaimed, ClaimedAt: now.Add(time.Second), ExpiresAt: now.Add(2 * time.Minute)}
	state := &State{
		ActiveLeaseByNode: map[string]string{},
		Nodes:             map[string]ResolverNode{"nod_1": {ID: "nod_1", State: NodeLeased}},
		Leases: map[string]NodeLease{
			older.ID: older,
			newer.ID: newer,
		},
	}

	RebuildLeaseOccupancyAt(state, now)

	if got := state.ActiveLeaseByNode["nod_1"]; got != newer.ID {
		t.Fatalf("active lease = %q, want %q", got, newer.ID)
	}
	if state.Leases[older.ID].State != LeaseExpired {
		t.Fatalf("older duplicate lease state = %s, want expired", state.Leases[older.ID].State)
	}
	if state.Leases[newer.ID].State != LeaseClaimed {
		t.Fatalf("newest lease state = %s, want claimed", state.Leases[newer.ID].State)
	}
}
