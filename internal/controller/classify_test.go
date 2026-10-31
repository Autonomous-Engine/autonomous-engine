package controller

import "testing"

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		lang, path string
		want       bool
	}{
		// Go
		{"", "pkg/add_test.go", true},
		{"", "pkg/add.go", false},
		{"", "pkg/testing_helpers.go", false},
		{"go", "internal/x/x_test.go", true},
		// TypeScript
		{"", "src/sum.test.ts", true},
		{"", "src/Button.test.tsx", true},
		{"", "src/sum.spec.ts", true},
		{"", "src/__tests__/sum.ts", true},
		{"", "src/sum.ts", false},
		{"", "src/testUtils.ts", false},
		{"", "src/contest.tsx", false},
		// JavaScript
		{"", "lib/a.test.js", true},
		{"", "lib/a.spec.mjs", true},
		{"", "lib/a.test.cjs", true},
		{"", "lib/a.js", false},
		{"", "lib/latest.js", false},
		// Python
		{"", "test_calc.py", true},
		{"", "pkg/calc_test.py", true},
		{"", "tests/conftest.py", true},
		{"", "calc.py", false},
		{"", "pkg/testing.py", false},
		// generic
		{"", "tests/fixtures/data.json", true},
		{"", "spec/models/user.spec.rb", true},
		{"", "./pkg/add_test.go", true},
		{"", `pkg\add_test.go`, true},
		{"", "", false},
		{"", "contests/a.go", false},
		// test/ (singular) for JS/TS and Python, not Go
		{"", "test/sum.js", true},
		{"", "web/test/helpers.ts", true},
		{"", "test/calc_check.py", true},
		{"", "test/fixtures/server.go", false},
		{"", "latest/a.js", false},
		// .spec. only on code extensions
		{"", "deploy.spec.yaml", false},
		{"", "openapi.spec.json", false},
		{"", "pkg/a.spec.py", true},
		{"", "lib/a.spec.ts", true},
	}
	for _, tc := range cases {
		if got := IsTestFile(tc.lang, tc.path); got != tc.want {
			t.Errorf("IsTestFile(%q, %q) = %t, want %t", tc.lang, tc.path, got, tc.want)
		}
	}
}

func TestLanguageOf(t *testing.T) {
	for p, want := range map[string]string{
		"a.go": LangGo, "a.ts": LangTypeScript, "a.tsx": LangTypeScript, "a.js": LangJavaScript,
		"a.mjs": LangJavaScript, "a.py": LangPython, "a.rb": "", "Makefile": "",
	} {
		if got := LanguageOf(p); got != want {
			t.Errorf("LanguageOf(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestIsTestCommand(t *testing.T) {
	cases := []struct {
		cmd, detected string
		want          bool
	}{
		// detected command, exact and with extra args
		{"go test ./... -count=1", "go test ./... -count=1", true},
		{"go test ./... -count=1 -run TestAdd", "go test ./... -count=1", true},
		{"make check", "make check", true},
		{"make check-all", "make check", false},
		// Go
		{"go test ./...", "", true},
		{"go test -run TestAdd ./pkg/", "", true},
		{"go test ./... 2>&1", "", true},
		{"cd svc && go test ./...", "", true},
		{"CGO_ENABLED=0 go test ./...", "", true},
		{"timeout 120 go test ./...", "", true},
		{"go build ./...", "", false},
		{"go vet ./...", "", false},
		// JS / TS
		{"npm test", "", true},
		{"npm test -- --run", "", true},
		{"npm run test", "", true},
		{"npm run test:unit", "", true},
		{"npm run build", "", false},
		{"yarn test", "", true},
		{"pnpm test", "", true},
		{"npx vitest run", "", true},
		{"npx --yes jest src/sum.test.ts", "", true},
		{"pnpm exec vitest run", "", true},
		{"yarn vitest", "", true},
		{"./node_modules/.bin/jest", "", true},
		{"npx tsc --noEmit", "", false},
		{"npm install", "", false},
		// M3-05 fast-check forms and their workspace-selector variants
		{"npm test --workspace=packages/lib -- src/math.test.js", "", true},
		{"npm test --workspace=packages/app", "", true},
		{"npm test -- packages/app/src/format.test.ts", "", true},
		{"npm -w packages/lib test", "", true},
		{"npm --workspace packages/lib run test", "", true},
		{"npm --prefix web test", "", true},
		{"npm -w packages/lib run build", "", false},
		{"npm -w packages/lib", "", false},
		{"pnpm --filter ./packages/a test", "", true},
		{"pnpm -F a exec vitest run", "", true},
		{"pnpm --filter ./packages/a run lint", "", false},
		{"yarn workspace @ws/lib test", "", true},
		{"yarn workspace @ws/lib run lint", "", false},
		{"node --test", "", true},
		{"node --test test/clock.test.js", "", true},
		{"node --test-reporter=spec --test test/a.test.mjs", "", true},
		{"node --import tsx --test", "", true},
		{"node --import tsx app.js", "", false},
		{"node test/clock.test.js", "", false},
		{"node scripts/lint.mjs", "", false},
		{"npm run lint --workspace=packages/app", "", false},
		{"npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "", false},
		{"python -m pytest -q tests/test_ops.py tests/test_fmt.py", "", true},
		{"python -m pytest tests/integration", "", true},
		{"python -m compileall -q src/calc/ops.py", "", false},
		{"python -m ruff check src/calc/ops.py", "", false},
		{"yarn test src/a.test.ts", "", true},
		// runs filtered by test name are not evidence
		{"node --test --test-name-pattern=add test/a.test.js", "", false},
		{"node --test --test-skip-pattern slow", "", false},
		{"node --test --test-only", "", false},
		{"npx jest -t add", "", false},
		{"npx vitest run -t add", "", false},
		{"npx jest --testNamePattern=add", "", false},
		{"npm test -- --testNamePattern add", "", false},
		{"npm test -- --testNamePattern add", "npm test", false},
		{"npx mocha -t 5000", "", true}, // mocha -t is the timeout
		{"npm test -- -t add", "", false},
		{"npm test -- -t=add", "", false},
		{"npm test -- -tadd", "", false},
		{"npm test -- -t add", "npm test", false},
		{"npm test --workspace=packages/app -- -t add src/a.test.ts", "", false},
		{"npm test -- -t 5000 --reporter mocha", "", true}, // mocha named: -t is its timeout
		{"npm test -- src/a.test.ts", "npm test", true},
		{"npx jest -tadd", "", false},
		{"npx jest -t smoke", "npx jest -t smoke", true}, // the configured command is trusted
		{"npx jest -t smoke --ci", "npx jest -t smoke", true},
		{"npm test --workspace=packages/lib -- 'src/routes/\\[id\\].test.js'", "", true},
		{"pnpm test src/a.test.ts", "", true},
		// Python
		{"pytest", "", true},
		{"pytest -q tests/test_calc.py", "", true},
		{"python -m pytest", "", true},
		{"python3 -m unittest discover", "", true},
		{"python setup.py test", "", false},
		{"python -m pip install pytest", "", false},
		// others
		{"cargo test", "", true},
		{"mvn -B test", "", true},
		{"./gradlew test", "", true},
		{"make test", "", true},
		{"dotnet test", "", true},
		{"bundle exec rspec", "", true},
		{"vendor/bin/phpunit", "", true},
		// exit status masked or not the runner's
		{"go test ./... | tail -20", "", false},
		{"go test ./... 2>&1 | tail -20", "", false},
		{"go test ./... || true", "", false},
		{"go test ./...; echo done", "", false},
		{"go test ./... &", "", false},
		{"npm test\necho ok", "", false},
		{"", "", false},
		{"echo go test", "", false},
		// configured compound commands run verbatim are evidence
		{"make lint; make test", "make lint; make test", true},
		{"make  lint;   make test", "make lint; make test", true},
		{"cd web && ./t.sh", "cd web && ./t.sh", true},
		{"cd web && ./scripts/test.sh", "./scripts/test.sh", true},
		{"go test ./... 2>&1 | tee out.log", "go test ./... 2>&1 | tee out.log", true},
		{"go test ./... | tee out.log", "go test ./... 2>&1 | tee out.log", true},
		// …but a configured command does not license added operators or a different pipe
		{"go test ./... | tail", "go test ./... -count=1", false},
		{"go test ./... -count=1 | tail", "go test ./... -count=1", false},
		{"go test ./... -count=1 || true", "go test ./... -count=1", false},
		{"make lint; make test; true", "make lint; make test", false},
		{"make lint", "make lint; make test", false},
		// runs that select, list or skip no tests are not evidence
		{"go test ./... -run NONE", "", false},
		{"go test -run=NONE ./...", "", false},
		{"go test -run '^$' ./...", "", false},
		{"go test ./... -count=1 -run NONE", "go test ./... -count=1", false},
		{"go test -list . ./...", "", false},
		{"npx jest --passWithNoTests", "", false},
		{"npx vitest run --passWithNoTests", "", false},
		{"mvn test -DskipTests", "", false},
		{"mvn -B test -Dmaven.test.skip=true", "", false},
		{"go test -run TestAdd ./...", "", true},
		// the configured command itself is trusted as written
		{"npx vitest run --passWithNoTests", "npx vitest run --passWithNoTests", true},
	}
	for _, tc := range cases {
		if got := IsTestCommand(tc.cmd, tc.detected); got != tc.want {
			t.Errorf("IsTestCommand(%q, %q) = %t, want %t", tc.cmd, tc.detected, got, tc.want)
		}
	}
}

func TestToolEventParsesArguments(t *testing.T) {
	ev := ToolEvent("c1", "write_file", `{"path":"a_test.go","content":"x"}`, false, "", nil)
	if ev.Path != "a_test.go" || ev.Kind != EventTool || ev.CallID != "c1" {
		t.Fatalf("write_file event = %+v", ev)
	}
	ev = ToolEvent("c2", "run_command", `{"command":"go test ./..."}`, false, "", exit(1))
	if ev.Command != "go test ./..." || *ev.ExitCode != 1 {
		t.Fatalf("run_command event = %+v", ev)
	}
	ev = ToolEvent("c3", "run_command", `{"command":"ignored"}`, false, "npm test", exit(0))
	if ev.Command != "npm test" {
		t.Fatalf("structured command must win: %+v", ev)
	}
	ev = ToolEvent("c4", "edit_file", `not json`, true, "", nil)
	if ev.Path != "" || !ev.IsError {
		t.Fatalf("bad args event = %+v", ev)
	}
}
