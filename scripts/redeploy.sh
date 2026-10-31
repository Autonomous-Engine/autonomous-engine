#!/usr/bin/env bash
# redeploy.sh — rebuild and restart a single service without breaking nginx DNS.
#
# Usage:
#   ./scripts/redeploy.sh dashboard-backend
#   ./scripts/redeploy.sh dashboard-frontend
#   ./scripts/redeploy.sh product-agent
#   ./scripts/redeploy.sh <any service from docker-compose>
#
# What it does:
#   1. Builds the new image
#   2. Stops + removes the old container
#   3. Starts the new container with correct env, networks, and ports
#   4. Verifies the container is running
#
# This replaces the manual docker run/stop/rm/network-connect/nginx-reload dance
# that kept causing 502s.

set -euo pipefail

SERVICE="${1:?Usage: $0 <service-name>}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# Load .env
set -a
source "$PROJECT_DIR/.env"
set +a

NETWORK="autonomous-engine_agent-net"
NETWORK2="autonomous-engine_default"

# Common env vars passed to every service
COMMON_ENV=(
  -e "DATABASE_URL=$DATABASE_URL"
  -e "KAFKA_BROKERS=$KAFKA_BROKERS"
  -e "JWT_SECRET=$JWT_SECRET"
  -e "MASTER_ENCRYPTION_KEY=$MASTER_ENCRYPTION_KEY"
)

build_go_service() {
  local svc="$1"
  local dockerfile="${2:-docker/Dockerfile.service}"
  echo "==> Building $svc ..."
  docker build -t "ae-$svc" -f "$PROJECT_DIR/$dockerfile" --build-arg "SERVICE=$svc" "$PROJECT_DIR"
}

build_frontend() {
  echo "==> Building dashboard-frontend ..."
  docker build -t ae-dashboard-frontend -f "$PROJECT_DIR/docker/Dockerfile.dashboard" "$PROJECT_DIR"
}

stop_and_remove() {
  local name="$1"
  echo "==> Stopping $name ..."
  docker stop "$name" 2>/dev/null || true
  docker rm "$name" 2>/dev/null || true
}

connect_networks() {
  local name="$1"
  # Ensure container is on both networks (agent-net for inter-service, default for legacy)
  docker network connect "$NETWORK" "$name" 2>/dev/null || true
  docker network connect "$NETWORK2" "$name" 2>/dev/null || true
}

verify_running() {
  local name="$1"
  sleep 2
  if docker ps --filter "name=$name" --format '{{.Status}}' | grep -q "Up"; then
    echo "==> $name is running."
  else
    echo "==> ERROR: $name failed to start. Logs:"
    docker logs "$name" --tail 20
    exit 1
  fi
}

case "$SERVICE" in

  dashboard-frontend)
    build_frontend
    stop_and_remove dashboard-frontend
    docker run -d \
      --name dashboard-frontend \
      --network "$NETWORK2" \
      -p "${DASHBOARD_UI_PORT:-32900}:3000" \
      ae-dashboard-frontend
    connect_networks dashboard-frontend
    verify_running dashboard-frontend
    ;;

  dashboard-backend)
    build_go_service dashboard-backend
    stop_and_remove dashboard-backend
    docker run -d \
      --name dashboard-backend \
      --network "$NETWORK" \
      -p "${DASHBOARD_PORT:-8080}:8080" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -e "QDRANT_URL=${QDRANT_URL:-}" \
      ae-dashboard-backend
    connect_networks dashboard-backend
    verify_running dashboard-backend
    ;;

  product-agent)
    build_go_service product-agent
    stop_and_remove product-agent
    docker run -d \
      --name product-agent \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -e "QDRANT_URL=${QDRANT_URL:-}" \
      -e "MAX_RETRY_COUNT=${MAX_RETRY_COUNT:-3}" \
      -e "HUMAN_REVIEW_REQUIRED=${HUMAN_REVIEW_REQUIRED:-true}" \
      -v autonomous-engine_repo-cache:/repo-cache:ro \
      ae-product-agent
    connect_networks product-agent
    verify_running product-agent
    ;;

  research-coordinator)
    build_go_service research-coordinator
    stop_and_remove research-coordinator
    docker run -d \
      --name research-coordinator \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -e "QDRANT_URL=${QDRANT_URL:-}" \
      -v autonomous-engine_repo-cache:/repo-cache:ro \
      ae-research-coordinator
    connect_networks research-coordinator
    verify_running research-coordinator
    ;;

  research-agent)
    build_go_service research-agent docker/Dockerfile.agent-lsp
    stop_and_remove research-agent
    docker run -d \
      --name research-agent \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -e "QDRANT_URL=${QDRANT_URL:-}" \
      -v autonomous-engine_repo-cache:/repo-cache:ro \
      ae-research-agent
    connect_networks research-agent
    verify_running research-agent
    ;;

  qa-agent)
    build_go_service qa-agent docker/Dockerfile.qa-agent
    stop_and_remove qa-agent
    docker run -d \
      --name qa-agent \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -v autonomous-engine_repo-cache:/repo-cache \
      ae-qa-agent
    connect_networks qa-agent
    verify_running qa-agent
    ;;

  review-agent)
    build_go_service review-agent
    stop_and_remove review-agent
    docker run -d \
      --name review-agent \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -e "QDRANT_URL=${QDRANT_URL:-}" \
      -v autonomous-engine_repo-cache:/repo-cache:ro \
      ae-review-agent
    connect_networks review-agent
    verify_running review-agent
    ;;

  gateway)
    build_go_service gateway
    stop_and_remove gateway
    docker run -d \
      --name gateway \
      --network "$NETWORK" \
      -p "${GATEWAY_PORT:-8082}:8082" \
      "${COMMON_ENV[@]}" \
      ae-gateway
    connect_networks gateway
    verify_running gateway
    ;;

  implementation-agent)
    build_go_service implementation-agent docker/Dockerfile.implementation-agent
    stop_and_remove implementation-agent
    docker run -d \
      --name implementation-agent \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -v autonomous-engine_repo-cache:/repo-cache \
      ae-implementation-agent
    connect_networks implementation-agent
    verify_running implementation-agent
    ;;

  implementation-planner)
    build_go_service implementation-planner
    stop_and_remove implementation-planner
    docker run -d \
      --name implementation-planner \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      -e "GITHUB_SERVICE_URL=${GITHUB_SERVICE_URL:-}" \
      -e "QDRANT_URL=${QDRANT_URL:-}" \
      -v autonomous-engine_repo-cache:/repo-cache:ro \
      ae-implementation-planner
    connect_networks implementation-planner
    verify_running implementation-planner
    ;;

  *)
    # Generic Go service — works for status-orchestrator, review-agent, etc.
    build_go_service "$SERVICE"
    stop_and_remove "$SERVICE"
    docker run -d \
      --name "$SERVICE" \
      --network "$NETWORK" \
      "${COMMON_ENV[@]}" \
      "ae-$SERVICE"
    connect_networks "$SERVICE"
    verify_running "$SERVICE"
    ;;

esac

echo "==> Done. $SERVICE redeployed successfully."
