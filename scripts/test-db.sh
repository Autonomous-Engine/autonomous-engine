#!/usr/bin/env bash
# Runs the whole Go suite — build, vet, and every test including the
# database-gated integration tests — against a disposable PostgreSQL in
# Docker, then removes it. One command, the same every time, so a change
# to one service is checked against every suite that shares its tables.
#
#   scripts/test-db.sh                 # everything
#   scripts/test-db.sh ./cmd/pr-coordinator/ -run Integration
#   AE_TEST_PG_KEEP=1 scripts/test-db.sh   # leave the database running
#
# The tests apply the migrations themselves. -p 1 because the packages share
# one database. A container named $AE_TEST_PG_NAME that already exists is
# reused and left running.
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=${AE_TEST_PG_NAME:-ae-test-pg}
PORT=${AE_TEST_PG_PORT:-5433}
IMAGE=${AE_TEST_PG_IMAGE:-postgres:16-alpine}
DSN="postgres://ae_test:ae_test@localhost:${PORT}/ae_test?sslmode=disable"

started=0
if ! docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
  echo "test-db: starting $NAME ($IMAGE) on port $PORT"
  docker run -d --name "$NAME" \
    -e POSTGRES_USER=ae_test -e POSTGRES_PASSWORD=ae_test -e POSTGRES_DB=ae_test \
    -p "${PORT}:5432" "$IMAGE" >/dev/null
  started=1
fi
cleanup() {
  if [ "$started" = 1 ] && [ -z "${AE_TEST_PG_KEEP:-}" ]; then
    echo "test-db: removing $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

for _ in $(seq 1 60); do
  if docker exec "$NAME" pg_isready -U ae_test -d ae_test >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$NAME" pg_isready -U ae_test -d ae_test >/dev/null

echo "test-db: go build ./... && go vet ./..."
go build ./... && go vet ./...

if [ "$#" -eq 0 ]; then
  set -- ./...
fi
echo "test-db: TEST_POSTGRES_DSN=<$NAME> go test -count=1 -p 1 $*"
TEST_POSTGRES_DSN="$DSN" go test -count=1 -p 1 "$@"
