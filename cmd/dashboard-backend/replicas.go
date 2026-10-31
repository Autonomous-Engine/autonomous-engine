package main

import (
	"fmt"
	"strconv"
	"strings"
)

// checkReplicaConfig returns how many dashboard-backend replicas the
// deployment runs, from DASHBOARD_REPLICAS (unset = 1), and refuses more than
// one without REDIS_URL.
//
// A process cannot see how many copies of itself a scheduler runs, so the
// deployment states it: the Helm chart sets DASHBOARD_REPLICAS from
// dashboardBackend.replicas, and docker compose runs one replica (the
// service has a fixed container_name). Each Kafka event reaches only one
// member of the dashboard's consumer groups; with several replicas and no
// Redis to relay it, clients connected to the other replicas would silently
// miss it, so start-up fails instead.
func checkReplicaConfig(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv("DASHBOARD_REPLICAS"))
	n := 1
	if raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return 0, fmt.Errorf("DASHBOARD_REPLICAS=%q: want a whole number of at least 1", raw)
		}
		n = v
	}
	if n > 1 && strings.TrimSpace(getenv("REDIS_URL")) == "" {
		return 0, fmt.Errorf("DASHBOARD_REPLICAS=%d but REDIS_URL is not set: with more than one replica, "+
			"WebSocket events consumed by one replica would never reach clients of the others. "+
			"Set REDIS_URL (Helm: redis.enabled=true) or run a single replica", n)
	}
	return n, nil
}
