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
	if state.ActiveLeaseByNode[lease.NodeID] == lease.ID {
		delete(state.ActiveLeaseByNode, lease.NodeID)
	}
}

func RebuildLeaseOccupancy(state *State) {
	RebuildLeaseOccupancyAt(state, time.Now().UTC())
}

func RebuildLeaseOccupancyAt(state *State, now time.Time) {
	now = now.UTC()
	activeByNode := make(map[string]NodeLease)
	for _, lease := range state.Leases {
		if lease.ID == "" || lease.NodeID == "" || !isActiveLeaseAt(lease, now) {
			continue
		}
		current, exists := activeByNode[lease.NodeID]
		if !exists || laterLease(lease, current) {
			activeByNode[lease.NodeID] = lease
		}
	}

	for leaseID, lease := range state.Leases {
		if lease.State != LeaseClaimed {
			continue
		}
		active, isActive := activeByNode[lease.NodeID]
		if isActive && active.ID == lease.ID {
			continue
		}

		// A lease can be missing from the active set because it has expired or
		// because an older duplicate claim lost recovery to the latest claim.
		lease.State = LeaseExpired
		finishedAt := now
		lease.FinishedAt = &finishedAt
		state.Leases[leaseID] = lease
	}

	occupancy := make(map[string]string, len(activeByNode))
	for nodeID, lease := range activeByNode {
		occupancy[nodeID] = lease.ID
	}
	state.ActiveLeaseByNode = occupancy

	for nodeID, node := range state.Nodes {
		_, occupied := activeByNode[nodeID]
		switch {
		case occupied && node.State != NodeLeased:
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

func laterLease(left NodeLease, right NodeLease) bool {
	if left.ClaimedAt.After(right.ClaimedAt) {
		return true
	}
	if left.ClaimedAt.Before(right.ClaimedAt) {
		return false
	}
	return left.ID > right.ID
}

func isActiveLeaseAt(lease NodeLease, now time.Time) bool {
	return lease.State == LeaseClaimed && lease.ExpiresAt.After(now)
}

func NodeLeaseOccupied(state *State, nodeID string) bool {
	if state.ActiveLeaseByNode == nil {
		return false
	}
	_, occupied := state.ActiveLeaseByNode[nodeID]
	return occupied
}

func LeaseIsCurrent(state *State, lease NodeLease) bool {
	if state.ActiveLeaseByNode == nil || lease.ID == "" || lease.NodeID == "" {
		return false
	}
	return state.ActiveLeaseByNode[lease.NodeID] == lease.ID
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
