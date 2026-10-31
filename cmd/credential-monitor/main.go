// credential-monitor periodically probes every stored GitHub + Jira
// credential, records the outcome, and publishes alerts on state changes
// and expiry thresholds. See:
//   - internal/credmonitor/ for the probe + runner + alert logic
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/credmonitor"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/health"
)

// healthPort follows the 80xx agent-health convention. 8090 = status-orchestrator,
// 8091-8100 = other agents, 8101 is next-free and used by credential-monitor.
const (
	agentID    = "credential-monitor"
	healthPort = 8101
)

func main() {
	log.SetPrefix("credential-monitor: ")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		log.Fatal("MASTER_ENCRYPTION_KEY not set; credential-monitor cannot decrypt credentials")
	}
	mk, err := crypto.LoadMasterKey(masterKeyStr)
	if err != nil {
		log.Fatalf("master key: %v", err)
	}
	cs := configstore.New(pool, mk)
	// Listen on pg_notify('config_changed') so UI updates to credentials/
	// endpoints flush the in-memory cache without a restart.
	cs.StartWatcher(ctx)

	kafka, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Printf("bus init failed (alerts will only persist): %v", err)
	}

	pub := &credmonitor.Publisher{DB: pool, Bus: kafka}

	interval := parseDurationEnv("CREDENTIAL_PROBE_INTERVAL", 15*time.Minute)
	probeTO := parseDurationEnv("CREDENTIAL_PROBE_TIMEOUT", 10*time.Second)

	r := &credmonitor.Runner{
		CS:        cs,
		Pub:       pub,
		Interval:  interval,
		ProbeTO:   probeTO,
		JiraBase:  cfg.Jira.URL, // still works as a default; ConfigStore base_url overrides
		JiraEmail: cfg.Jira.Email,
	}

	// ── Health server ─────────────────────────────────────────────────────────
	// Before this, credential-monitor had no /health at all — Docker health
	// checks and uptime monitors saw "no endpoint" and could not detect a
	// crashed runner. The checks below mirror the pattern every other agent
	// uses so the fleet is uniform. Kafka is included because a partitioned
	// cluster silently disables the alert-publish path.
	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	if kafka != nil {
		checker.AddCheck("kafka", kafka.HealthCheck())
	} else {
		// Record the init failure in /health so operators can see it instead
		// of silently degrading to DB-only alerts.
		checker.AddCheck("kafka", func() error {
			return fmt.Errorf("kafka bus unavailable (init failed at startup)")
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server error: %v", err)
		}
	}()

	log.Printf("starting runner interval=%s probe_timeout=%s", interval, probeTO)
	r.Start(ctx)

	// Runner.Start returns when ctx is cancelled; shut the health server down
	// cleanly so in-flight /health requests finish.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("health server shutdown error: %v", err)
	}

	log.Println("shut down")
}

func parseDurationEnv(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warning: %s=%q invalid, using %s", name, v, fallback)
		return fallback
	}
	return d
}
