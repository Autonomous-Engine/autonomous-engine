// This module exists only to keep the fixture hidden tests out of the
// autonomous-engine root module. The *_test.go files under tasks/*/hidden-tests/
// belong to the benchmark target repositories (package gjson, cron, ...) and do
// not compile here; a nested go.mod makes `go build/vet/test ./...` from the
// repository root skip this subtree. Nothing in this directory is built directly.
module github.com/autonomousengine/autonomous-engine/benchmarks/tasks

go 1.22
