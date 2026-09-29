package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"time"
)

func ActivatePlan(plan *RolloutPlan, now time.Time) error {
	if plan.State != PlanDraft {
		return InvalidState("plan can be activated only from draft", map[string]any{
			"plan_id": plan.ID,
			"state":   plan.State,
		})
	}
	if plan.LatestRevisionID == "" {
		return InvalidState("plan needs a zone revision before activation", map[string]any{
			"plan_id": plan.ID,
		})
	}
	plan.State = PlanActive
	plan.ActivatedAt = timePointer(now.UTC())
	plan.Version++
	return nil
}

func CompletePlan(plan *RolloutPlan, now time.Time) error {
	if plan.State != PlanActive {
		return InvalidState("plan can be completed only from active", map[string]any{
			"plan_id": plan.ID,
			"state":   plan.State,
		})
	}
	plan.State = PlanCompleted
	plan.CompletedAt = timePointer(now.UTC())
	plan.Version++
	return nil
}

func RecordLeaseClaim(state *State, lease NodeLease) {
	if state.ActiveLeaseByNode == nil {
		state.ActiveLeaseByNode = make(map[string]string)
	}
	if lease.ID == "" || lease.NodeID == "" {
		return
	}
	state.ActiveLeaseByNode[lease.NodeID] = lease.ID
}

func RecordLeaseFinished(state *State, lease NodeLease) {
	if state.ActiveLeaseByNode == nil {
		return
	}
	if lease.ID == "" || lease.NodeID == "" {
		return
	}
	// Only clear the occupancy while this lease is still the one holding the
	// node. A newer claim for the same node must keep the node isolated.
	if active, exists := state.ActiveLeaseByNode[lease.NodeID]; exists && active == lease.ID {
		delete(state.ActiveLeaseByNode, lease.NodeID)
	}
}

// RebuildLeaseOccupancy rebuilds the node occupancy index from the lease set
// after a reload. Node availability is driven exclusively by leases that are
// still claimed at now: reported, released and expired leases never block the
// node, while a live claimed lease keeps it isolated. A node stays isolated as
// long as any of its leases is still claimed, even when a newer lease for the
// same node has already ended. It also reconciles lease and node states for
// claims that reached their deadline while the service was stopped.
func RebuildLeaseOccupancy(state *State, now time.Time) {
	active := make(map[string]string)
	for _, lease := range state.Leases {
		if lease.ID == "" || lease.NodeID == "" || lease.State != LeaseClaimed {
			continue
		}
		if !lease.ExpiresAt.After(now) {
			finishDueLease(state, lease, now)
			continue
		}
		current, exists := active[lease.NodeID]
		if !exists || lease.ID > current {
			active[lease.NodeID] = lease.ID
		}
	}
	state.ActiveLeaseByNode = active
	reconcileNodeStates(state, now)
}

func finishDueLease(state *State, lease NodeLease, now time.Time) {
	finishedAt := now
	lease.State = LeaseExpired
	lease.FinishedAt = &finishedAt
	state.Leases[lease.ID] = lease
}

func reconcileNodeStates(state *State, now time.Time) {
	for nodeID, node := range state.Nodes {
		_, occupied := state.ActiveLeaseByNode[nodeID]
		switch {
		case occupied && node.State == NodeReady:
			node.State = NodeLeased
			node.Version++
			node.UpdatedAt = now
			state.Nodes[nodeID] = node
		case !occupied && node.State == NodeLeased:
			node.State = NodeReady
			node.Version++
			node.UpdatedAt = now
			state.Nodes[nodeID] = node
		}
	}
}

func NodeLeaseOccupied(state *State, nodeID string) bool {
	if state.ActiveLeaseByNode == nil {
		return false
	}
	_, occupied := state.ActiveLeaseByNode[nodeID]
	return occupied
}

func ActiveLeaseCount(state *State) int {
	if state.ActiveLeaseByNode == nil {
		return 0
	}
	return len(state.ActiveLeaseByNode)
}

func LeaseFingerprint(planID string, input LeaseInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%s|%d|%d",
		planID,
		input.RequestKey,
		input.Region,
		input.LeaseSeconds,
		input.MaxNodes,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func ProbeFingerprint(leaseID string, input ProbeInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%t|%d|%d|%s",
		leaseID,
		input.RequestKey,
		input.Success,
		input.ObservedSerial,
		input.LatencyMS,
		input.ObservedAt.UTC().Format(time.RFC3339Nano),
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func BuildAssessment(
	id string,
	plan RolloutPlan,
	revision ZoneRevision,
	probes []Probe,
	now time.Time,
) RolloutAssessment {
	reasons := make(map[string]struct{})
	if len(probes) == 0 {
		reasons["no_probes"] = struct{}{}
	}
	probeCount := len(probes)
	successCount := 0
	maximumLatency := 0
	nodes := make(map[string]struct{})
	for _, probe := range probes {
		nodes[probe.NodeID] = struct{}{}
		if probe.LatencyMS > maximumLatency {
			maximumLatency = probe.LatencyMS
		}
		if probe.Success {
			successCount++
			if probe.ObservedSerial != revision.Serial {
				reasons["serial_mismatch"] = struct{}{}
			}
		}
	}
	if len(nodes) < plan.ExpectedNodes {
		reasons["node_not_observed"] = struct{}{}
	}
	successRatio := 0.0
	if probeCount > 0 {
		successRatio = round(float64(successCount)/float64(probeCount), 6)
	}
	if successRatio < plan.MinimumSuccessRatio {
		reasons["success_ratio_below_minimum"] = struct{}{}
	}
	if maximumLatency > plan.MaximumLatencyMS {
		reasons["latency_limit_exceeded"] = struct{}{}
	}
	reasonList := make([]string, 0, len(reasons))
	for reason := range reasons {
		reasonList = append(reasonList, reason)
	}
	sort.Strings(reasonList)
	verdict := VerdictConsistent
	if len(reasonList) > 0 {
		verdict = VerdictReviewRequired
	}
	return RolloutAssessment{
		ID:               id,
		PlanID:           plan.ID,
		RevisionID:       revision.ID,
		Verdict:          verdict,
		Reasons:          reasonList,
		ProbeCount:       probeCount,
		SuccessCount:     successCount,
		SuccessRatio:     successRatio,
		MaximumLatencyMS: maximumLatency,
		CreatedAt:        now.UTC(),
	}
}

func round(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func timePointer(value time.Time) *time.Time {
	return &value
}
