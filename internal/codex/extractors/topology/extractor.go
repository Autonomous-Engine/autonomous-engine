package topology

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// Extractor assigns topology roles to repos based on filesystem signals.
// It is rule-based (KindStatic) and runs after sibling extractors so that
// the presence of certain blobs (component_library, api_style, events, etc.)
// can inform the classification.  In practice, the current implementation
// reads the filesystem directly rather than querying sibling blobs, keeping
// the extractor self-contained for the initial release.
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "product_topology" }
func (*Extractor) Family() string       { return "topology" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

// CaresAbout always returns true because topology classification depends on
// the aggregate structure of the repo, not on individual file changes.
func (*Extractor) CaresAbout(_ *framework.GitDiff) bool { return true }

// ---------------------------------------------------------------------------
// Output types
// ---------------------------------------------------------------------------

// RoleAssignment is a single role classification for a repo.
type RoleAssignment struct {
	Role       string   `json:"role"`
	Confidence float64  `json:"confidence"`
	Signals    []string `json:"signals"`
}

// TopologyOutput is the blob payload.
type TopologyOutput struct {
	Roles []RoleAssignment `json:"roles"`
}

// ---------------------------------------------------------------------------
// Extraction entry-points
// ---------------------------------------------------------------------------

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	roles := classifyRepo(ec.RepoPath)
	output := TopologyOutput{Roles: roles}
	payload, _ := json.Marshal(output)

	confidence := 0.85
	if len(roles) == 1 && roles[0].Role == "service" {
		confidence = 0.5
	}

	var evidence []framework.Evidence
	for _, r := range roles {
		for _, s := range r.Signals {
			evidence = append(evidence, framework.Evidence{Note: s})
		}
	}

	return []*framework.Blob{{
		Section:    "product_topology",
		Payload:    payload,
		Confidence: confidence,
		Evidence:   evidence,
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

// ---------------------------------------------------------------------------
// Classification rules
// ---------------------------------------------------------------------------

func classifyRepo(root string) []RoleAssignment {
	var roles []RoleAssignment

	if r, ok := detectFrontend(root); ok {
		roles = append(roles, r)
	}
	if r, ok := detectAPI(root); ok {
		roles = append(roles, r)
	}
	if r, ok := detectWorker(root); ok {
		roles = append(roles, r)
	}
	if r, ok := detectSDK(root); ok {
		roles = append(roles, r)
	}
	if r, ok := detectSharedLib(root); ok {
		roles = append(roles, r)
	}
	if r, ok := detectInfra(root); ok {
		roles = append(roles, r)
	}

	if len(roles) == 0 {
		roles = append(roles, RoleAssignment{
			Role:       "service",
			Confidence: 0.5,
			Signals:    []string{"default_fallback"},
		})
	}

	return roles
}

// ---------------------------------------------------------------------------
// Individual detectors
// ---------------------------------------------------------------------------

// detectFrontend checks for app/ or pages/ directory combined with frontend
// dependency markers in package.json.
func detectFrontend(root string) (RoleAssignment, bool) {
	var signals []string

	hasAppDir := dirExists(root, "app")
	hasPagesDir := dirExists(root, "pages")
	if hasAppDir {
		signals = append(signals, "app_dir_exists")
	}
	if hasPagesDir {
		signals = append(signals, "pages_dir_exists")
	}
	if !hasAppDir && !hasPagesDir {
		return RoleAssignment{}, false
	}

	// Look for frontend frameworks in package.json.
	frontendDeps := []string{"react", "next", "vue", "nuxt", "@angular/core", "svelte"}
	if deps := readPackageJSONDeps(root); deps != nil {
		for _, fd := range frontendDeps {
			if _, ok := deps[fd]; ok {
				signals = append(signals, "package_dep:"+fd)
			}
		}
	}

	// Also consider CSS-related signals.
	cssFiles := []string{"tailwind.config.js", "tailwind.config.ts", "postcss.config.js"}
	for _, f := range cssFiles {
		if fileExists(root, f) {
			signals = append(signals, "css_config:"+f)
		}
	}

	// Need at least a directory signal plus one dependency/css signal to qualify.
	if len(signals) < 2 {
		return RoleAssignment{}, false
	}

	conf := 0.7 + float64(min(len(signals), 4))*0.05
	if conf > 0.95 {
		conf = 0.95
	}
	return RoleAssignment{Role: "frontend", Confidence: conf, Signals: signals}, true
}

// detectAPI checks for Go or Node.js API framework indicators combined with
// server entry-points.
func detectAPI(root string) (RoleAssignment, bool) {
	var signals []string

	// Go API signals.
	if fileExists(root, "go.mod") {
		signals = append(signals, "go.mod_exists")
		goMod := readFile(root, "go.mod")
		goFrameworks := []string{
			"github.com/gin-gonic/gin",
			"github.com/labstack/echo",
			"github.com/go-chi/chi",
			"github.com/gofiber/fiber",
			"github.com/gorilla/mux",
			"net/http",
		}
		for _, fw := range goFrameworks {
			if strings.Contains(goMod, fw) {
				signals = append(signals, "go_framework:"+fw)
			}
		}
		// cmd/server pattern.
		if dirExists(root, "cmd/server") || dirExists(root, "cmd/api") {
			signals = append(signals, "cmd_server_dir")
		}
		if fileExists(root, "cmd/server/main.go") || fileExists(root, "cmd/api/main.go") {
			signals = append(signals, "server_main_go")
		}
	}

	// Node.js API signals.
	if deps := readPackageJSONDeps(root); deps != nil {
		apiDeps := []string{"express", "fastify", "koa", "@nestjs/core", "@trpc/server", "hapi", "@hapi/hapi"}
		for _, d := range apiDeps {
			if _, ok := deps[d]; ok {
				signals = append(signals, "node_framework:"+d)
			}
		}
	}

	// Proto files are a strong API signal.
	if hasFilesWithExt(root, ".proto") {
		signals = append(signals, "proto_files")
	}

	if len(signals) < 2 {
		return RoleAssignment{}, false
	}

	conf := 0.7 + float64(min(len(signals), 4))*0.05
	if conf > 0.95 {
		conf = 0.95
	}
	return RoleAssignment{Role: "api", Confidence: conf, Signals: signals}, true
}

// detectWorker checks for event consumer markers without HTTP server indicators.
func detectWorker(root string) (RoleAssignment, bool) {
	var signals []string

	hasHTTP := false
	if fileExists(root, "go.mod") {
		goMod := readFile(root, "go.mod")
		httpFrameworks := []string{
			"github.com/gin-gonic/gin",
			"github.com/labstack/echo",
			"github.com/go-chi/chi",
			"github.com/gofiber/fiber",
			"github.com/gorilla/mux",
		}
		for _, fw := range httpFrameworks {
			if strings.Contains(goMod, fw) {
				hasHTTP = true
				break
			}
		}
	}
	if deps := readPackageJSONDeps(root); deps != nil {
		httpDeps := []string{"express", "fastify", "koa", "@nestjs/core", "@hapi/hapi"}
		for _, d := range httpDeps {
			if _, ok := deps[d]; ok {
				hasHTTP = true
				break
			}
		}
	}

	// Event/queue dependency signals.
	eventDeps := map[string]string{
		"kafkajs":                           "kafkajs",
		"bull":                              "bull",
		"bullmq":                            "bullmq",
		"amqplib":                           "amqplib",
		"github.com/segmentio/kafka-go":     "kafka-go",
		"github.com/confluentinc/confluent-kafka-go": "confluent-kafka",
		"github.com/rabbitmq/amqp091-go":             "amqp091-go",
	}

	if deps := readPackageJSONDeps(root); deps != nil {
		for dep, label := range eventDeps {
			if _, ok := deps[dep]; ok {
				signals = append(signals, "event_dep:"+label)
			}
		}
	}
	if fileExists(root, "go.mod") {
		goMod := readFile(root, "go.mod")
		for dep, label := range eventDeps {
			if strings.Contains(goMod, dep) {
				signals = append(signals, "event_dep:"+label)
			}
		}
	}

	// cmd/worker pattern.
	if dirExists(root, "cmd/worker") || dirExists(root, "cmd/consumer") {
		signals = append(signals, "cmd_worker_dir")
	}

	if len(signals) == 0 {
		return RoleAssignment{}, false
	}
	if hasHTTP {
		// Has HTTP framework, so this is not a pure worker.
		return RoleAssignment{}, false
	}

	conf := 0.7 + float64(min(len(signals), 4))*0.05
	if conf > 0.95 {
		conf = 0.95
	}
	return RoleAssignment{Role: "worker", Confidence: conf, Signals: signals}, true
}

// detectSDK checks for package.json with a name field and no app/ directory,
// indicating a publishable library/SDK rather than an application.
func detectSDK(root string) (RoleAssignment, bool) {
	if dirExists(root, "app") {
		return RoleAssignment{}, false
	}

	pkgData := readFile(root, "package.json")
	if pkgData == "" {
		return RoleAssignment{}, false
	}

	var pkg struct {
		Name    string `json:"name"`
		Main    string `json:"main"`
		Module  string `json:"module"`
		Types   string `json:"types"`
		Exports any    `json:"exports"`
	}
	if json.Unmarshal([]byte(pkgData), &pkg) != nil {
		return RoleAssignment{}, false
	}
	if pkg.Name == "" {
		return RoleAssignment{}, false
	}

	var signals []string
	signals = append(signals, "package_name:"+pkg.Name)

	if pkg.Main != "" {
		signals = append(signals, "main_field")
	}
	if pkg.Module != "" {
		signals = append(signals, "module_field")
	}
	if pkg.Types != "" {
		signals = append(signals, "types_field")
	}
	if pkg.Exports != nil {
		signals = append(signals, "exports_field")
	}

	// Scoped packages are a strong SDK signal.
	if strings.HasPrefix(pkg.Name, "@") {
		signals = append(signals, "scoped_package")
	}

	// src/index.ts is a common SDK entry-point.
	if fileExists(root, "src/index.ts") || fileExists(root, "src/index.js") {
		signals = append(signals, "src_index_entry")
	}

	if len(signals) < 2 {
		return RoleAssignment{}, false
	}

	conf := 0.7 + float64(min(len(signals), 4))*0.05
	if conf > 0.95 {
		conf = 0.95
	}
	return RoleAssignment{Role: "sdk", Confidence: conf, Signals: signals}, true
}

// detectSharedLib checks for go.mod in a pkg/ or shared/ directory pattern.
func detectSharedLib(root string) (RoleAssignment, bool) {
	if !fileExists(root, "go.mod") {
		return RoleAssignment{}, false
	}

	var signals []string

	goMod := readFile(root, "go.mod")
	// Module path containing /pkg/ or /shared/ is a strong signal.
	lines := strings.Split(goMod, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "module ") {
			modPath := strings.TrimPrefix(line, "module ")
			modPath = strings.TrimSpace(modPath)
			if strings.Contains(modPath, "/pkg/") || strings.Contains(modPath, "/shared/") {
				signals = append(signals, "module_path_shared")
			}
			break
		}
	}

	if dirExists(root, "pkg") {
		signals = append(signals, "pkg_dir_exists")
	}
	if dirExists(root, "shared") {
		signals = append(signals, "shared_dir_exists")
	}

	// No cmd/ directory suggests it is not a standalone service.
	if !dirExists(root, "cmd") {
		signals = append(signals, "no_cmd_dir")
	}

	if len(signals) < 2 {
		return RoleAssignment{}, false
	}

	conf := 0.7 + float64(min(len(signals), 4))*0.05
	if conf > 0.95 {
		conf = 0.95
	}
	return RoleAssignment{Role: "shared_lib", Confidence: conf, Signals: signals}, true
}

// detectInfra checks for a Dockerfile targeting infrastructure images with
// no significant application code.
func detectInfra(root string) (RoleAssignment, bool) {
	if !fileExists(root, "Dockerfile") {
		return RoleAssignment{}, false
	}

	var signals []string

	dockerfile := readFile(root, "Dockerfile")
	infraImages := []string{"nginx", "redis", "postgres", "mysql", "mongo", "traefik", "haproxy", "envoy", "vault"}
	for _, img := range infraImages {
		if strings.Contains(strings.ToLower(dockerfile), "from "+img) || strings.Contains(strings.ToLower(dockerfile), "from docker.io/"+img) {
			signals = append(signals, "infra_base_image:"+img)
		}
	}

	if len(signals) == 0 {
		return RoleAssignment{}, false
	}

	// Check that there is no significant app code alongside.
	hasGoCode := fileExists(root, "main.go") || dirExists(root, "cmd") || dirExists(root, "internal")
	hasTSCode := fileExists(root, "src/index.ts") || fileExists(root, "src/index.js")
	if hasGoCode || hasTSCode {
		return RoleAssignment{}, false
	}

	signals = append(signals, "no_app_code")

	conf := 0.8
	return RoleAssignment{Role: "infra", Confidence: conf, Signals: signals}, true
}

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

func dirExists(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, rel))
	return err == nil && info.IsDir()
}

func fileExists(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, rel))
	return err == nil && !info.IsDir()
}

func readFile(root, rel string) string {
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return ""
	}
	return string(data)
}

// readPackageJSONDeps returns a merged map of dependencies + devDependencies.
func readPackageJSONDeps(root string) map[string]string {
	data := readFile(root, "package.json")
	if data == "" {
		return nil
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal([]byte(data), &pkg) != nil {
		return nil
	}
	merged := make(map[string]string, len(pkg.Dependencies)+len(pkg.DevDependencies))
	for k, v := range pkg.Dependencies {
		merged[k] = v
	}
	for k, v := range pkg.DevDependencies {
		merged[k] = v
	}
	return merged
}

// hasFilesWithExt does a shallow walk looking for files with the given extension.
// It stops after finding the first match.
func hasFilesWithExt(root, ext string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ext) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
