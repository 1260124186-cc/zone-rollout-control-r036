package workflowprobe

import (
	"net/http"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

func listNodesByState(harness *harness, stateFilter string) ([]domain.ResolverNode, error) {
	var page domain.NodePage
	path := "/v1/nodes?limit=100"
	if stateFilter != "" {
		path += "&state=" + stateFilter
	}
	err := harness.call(http.MethodGet, path, nil, &page)
	return page.Items, err
}

func runLeaseRestartRecovery() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	const nodeCount = 3
	for _, name := range []string{
		"resolver-restart-01",
		"resolver-restart-02",
		"resolver-restart-03",
	} {
		if _, err := enrollNode(harness, name, "north"); err != nil {
			return err
		}
	}
	plan, revision, err := createActivePlan(harness, "restart.example", 3001, nodeCount, 0.5, 100)
	if err != nil {
		return err
	}

	// Two nodes get claimed; one is released and the other reports completion.
	first, err := allocate(harness, plan.ID, "restart-lease-0001", "north", 2)
	if err != nil {
		return err
	}
	if len(first.LeaseIDs) != 2 {
		return ensure(false, "first lease group must contain two leases")
	}
	var released domain.NodeLease
	if err := harness.call(http.MethodPatch, "/v1/leases/"+first.LeaseIDs[0]+"/release", nil, &released); err != nil {
		return err
	}
	if released.State != domain.LeaseReleased {
		return ensure(false, "lease release did not update state")
	}
	if _, err := submitProbe(harness, first.LeaseIDs[1], "restart-probe-0001", true, revision.Serial, 10); err != nil {
		return err
	}

	// After a restart, released and reported leases must not keep nodes occupied.
	if err := harness.restart(); err != nil {
		return err
	}
	second, err := allocate(harness, plan.ID, "restart-lease-0002", "north", nodeCount)
	if err != nil {
		return err
	}
	if len(second.LeaseIDs) != nodeCount {
		return ensure(false, "finished leases must release every node after restart")
	}

	// Claims that are still live must survive a restart and keep nodes isolated,
	// so another allocation cannot claim the same nodes.
	if err := harness.restart(); err != nil {
		return err
	}
	_, err = allocate(harness, plan.ID, "restart-lease-0003", "north", nodeCount)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeNodeUnavailable)); err != nil {
		return err
	}
	leased, err := listNodesByState(harness, string(domain.NodeLeased))
	if err != nil {
		return err
	}
	if len(leased) != nodeCount {
		return ensure(false, "live leases must keep every node marked leased after restart")
	}
	ready, err := listNodesByState(harness, string(domain.NodeReady))
	if err != nil {
		return err
	}
	if len(ready) != 0 {
		return ensure(false, "no node held by a live lease may be listed as ready")
	}

	// Repeated restarts must not resurrect ended leases or release live ones.
	if err := harness.restart(); err != nil {
		return err
	}
	if err := harness.restart(); err != nil {
		return err
	}
	_, err = allocate(harness, plan.ID, "restart-lease-0004", "north", nodeCount)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeNodeUnavailable)); err != nil {
		return err
	}

	// Releasing the live leases in the running service must make the nodes
	// reusable without any restart.
	for _, leaseID := range second.LeaseIDs {
		if err := harness.call(http.MethodPatch, "/v1/leases/"+leaseID+"/release", nil, nil); err != nil {
			return err
		}
	}
	third, err := allocate(harness, plan.ID, "restart-lease-0005", "north", nodeCount)
	if err != nil {
		return err
	}
	if len(third.LeaseIDs) != nodeCount {
		return ensure(false, "released leases must free nodes for reuse immediately")
	}

	// Leases that reach their deadline while the service is stopped expire during
	// recovery and must not block reuse; node state must reconcile back to ready.
	harness.clock.Advance(601 * time.Second)
	if err := harness.restart(); err != nil {
		return err
	}
	var expired domain.NodeLease
	if err := harness.call(http.MethodPatch, "/v1/leases/"+third.LeaseIDs[0]+"/release", nil, &expired); err != nil {
		return err
	}
	if expired.State != domain.LeaseExpired {
		return ensure(false, "a due lease must be treated as expired after restart")
	}
	leased, err = listNodesByState(harness, string(domain.NodeLeased))
	if err != nil {
		return err
	}
	if len(leased) != 0 {
		return ensure(false, "expired leases must leave nodes ready after restart")
	}
	ready, err = listNodesByState(harness, string(domain.NodeReady))
	if err != nil {
		return err
	}
	if len(ready) != nodeCount {
		return ensure(false, "all nodes with expired leases must be listed as ready")
	}
	fourth, err := allocate(harness, plan.ID, "restart-lease-0006", "north", nodeCount)
	if err != nil {
		return err
	}
	if len(fourth.LeaseIDs) != nodeCount {
		return ensure(false, "expired leases must not block node reuse after restart")
	}

	// The freshly claimed leases stay isolated through another restart.
	if err := harness.restart(); err != nil {
		return err
	}
	_, err = allocate(harness, plan.ID, "restart-lease-0007", "north", nodeCount)
	return expectFailure(err, http.StatusConflict, string(domain.CodeNodeUnavailable))
}
