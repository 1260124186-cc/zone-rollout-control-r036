package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"solo-0014-zone-rollout-control/internal/allocation"
	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type fixedIDs struct {
	next int
}

func (ids *fixedIDs) New(prefix string) string {
	id := prefix + "_" + strconv.Itoa(ids.next)
	ids.next++
	return id
}

func TestRecoveredReleasedLeaseDoesNotBlockAllocation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	clock := platform.NewManualClock(now)
	path := filepath.Join(t.TempDir(), "state.json")
	stateStore, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedLeasableNode(ctx, stateStore, now); err != nil {
		t.Fatal(err)
	}

	ids := &fixedIDs{}
	service := allocation.New(stateStore, clock, ids)
	first, err := service.Allocate(ctx, "plan_1", domain.LeaseInput{
		RequestKey:   "request_1",
		Region:       "east",
		LeaseSeconds: 60,
		MaxNodes:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(ctx, first.LeaseIDs[0]); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if domain.NodeLeaseOccupied(&recovered, "nod_1") {
		t.Fatal("released lease must not occupy its node after restart")
	}
	if recovered.Nodes["nod_1"].State != domain.NodeReady {
		t.Fatalf("recovered node state = %s, want ready", recovered.Nodes["nod_1"].State)
	}

	service = allocation.New(reopened, clock, ids)
	second, err := service.Allocate(ctx, "plan_1", domain.LeaseInput{
		RequestKey:   "request_2",
		Region:       "east",
		LeaseSeconds: 60,
		MaxNodes:     1,
	})
	if err != nil {
		t.Fatalf("released node should be allocatable after restart: %v", err)
	}
	if second.LeaseIDs[0] == first.LeaseIDs[0] {
		t.Fatal("new allocation must create a replacement lease")
	}
}

func TestRecoveredExpiredLeaseDoesNotBlockAllocation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	clock := platform.NewManualClock(now)
	path := filepath.Join(t.TempDir(), "state.json")
	stateStore, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedLeasableNode(ctx, stateStore, now); err != nil {
		t.Fatal(err)
	}

	ids := &fixedIDs{}
	first, err := allocation.New(stateStore, clock, ids).Allocate(ctx, "plan_1", domain.LeaseInput{
		RequestKey:   "request_1",
		Region:       "east",
		LeaseSeconds: 60,
		MaxNodes:     1,
	})
	if err != nil {
		t.Fatal(err)
	}

	later := now.Add(2 * time.Minute)
	clock.Advance(2 * time.Minute)
	reopened, err := store.OpenAt(path, later)
	if err != nil {
		t.Fatal(err)
	}
	second, err := allocation.New(reopened, clock, ids).Allocate(ctx, "plan_1", domain.LeaseInput{
		RequestKey:   "request_2",
		Region:       "east",
		LeaseSeconds: 60,
		MaxNodes:     1,
	})
	if err != nil {
		t.Fatalf("expired node should be allocatable after restart: %v", err)
	}
	snapshot, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Leases[first.LeaseIDs[0]].State != domain.LeaseExpired {
		t.Fatalf("old lease state = %s, want expired", snapshot.Leases[first.LeaseIDs[0]].State)
	}
	if snapshot.ActiveLeaseByNode["nod_1"] != second.LeaseIDs[0] {
		t.Fatalf("active lease = %q, want %q", snapshot.ActiveLeaseByNode["nod_1"], second.LeaseIDs[0])
	}
}

func TestRecoveredReportedLeaseDoesNotBlockAllocation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	stateStore, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedLeasableNode(ctx, stateStore, now); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Write(ctx, func(state *domain.State) error {
		finishedAt := now
		state.Nodes["nod_1"].State = domain.NodeLeased
		state.Leases["lse_reported"] = domain.NodeLease{
			ID:         "lse_reported",
			PlanID:     "plan_1",
			NodeID:     "nod_1",
			State:      domain.LeaseReported,
			ClaimedAt:  now.Add(-time.Minute),
			ExpiresAt:  now.Add(time.Minute),
			FinishedAt: &finishedAt,
			ProbeID:    "prb_1",
		}
		state.ActiveLeaseByNode["nod_1"] = "lse_reported"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	group, err := allocation.New(reopened, platform.NewManualClock(now), &fixedIDs{}).Allocate(ctx, "plan_1", domain.LeaseInput{
		RequestKey:   "request_after_report",
		Region:       "east",
		LeaseSeconds: 60,
		MaxNodes:     1,
	})
	if err != nil {
		t.Fatalf("reported node should be allocatable after restart: %v", err)
	}
	if len(group.LeaseIDs) != 1 {
		t.Fatalf("allocated lease count = %d, want 1", len(group.LeaseIDs))
	}
}

func TestRecoveredActiveLeaseStillBlocksAllocation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	stateStore, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedLeasableNode(ctx, stateStore, now); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Write(ctx, func(state *domain.State) error {
		state.Nodes["nod_1"].State = domain.NodeReady
		state.Leases["lse_active"] = domain.NodeLease{
			ID:        "lse_active",
			PlanID:    "plan_1",
			NodeID:    "nod_1",
			State:     domain.LeaseClaimed,
			ClaimedAt: now.Add(-time.Minute),
			ExpiresAt: now.Add(time.Minute),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenAt(path, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Nodes["nod_1"].State != domain.NodeLeased {
		t.Fatalf("recovered node state = %s, want leased", snapshot.Nodes["nod_1"].State)
	}
	_, err = allocation.New(reopened, platform.NewManualClock(now), &fixedIDs{}).Allocate(ctx, "plan_1", domain.LeaseInput{
		RequestKey:   "request_during_active_lease",
		Region:       "east",
		LeaseSeconds: 60,
		MaxNodes:     1,
	})
	if domain.ErrorCodeOf(err) != domain.CodeNodeUnavailable {
		t.Fatalf("allocation error = %v, want %s", err, domain.CodeNodeUnavailable)
	}
}

func TestOpenCreatesSnapshotWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := store.Open(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot was not initialized: %v", err)
	}
}

func seedLeasableNode(ctx context.Context, stateStore *store.Store, now time.Time) error {
	return stateStore.Write(ctx, func(state *domain.State) error {
		state.Plans["plan_1"] = domain.RolloutPlan{
			ID:                  "plan_1",
			State:               domain.PlanActive,
			MinimumSuccessRatio: 1,
			MaximumLatencyMS:    100,
			ExpectedNodes:       1,
			Version:             1,
			CreatedAt:           now,
		}
		state.Nodes["nod_1"] = domain.ResolverNode{
			ID:        "nod_1",
			NodeName:  "resolver-east-01",
			Region:    "east",
			State:     domain.NodeReady,
			Version:   1,
			CreatedAt: now,
			UpdatedAt: now,
		}
		return nil
	})
}
