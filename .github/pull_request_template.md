## What and why

<!-- One or two sentences on the change and the problem it solves. Link the issue. -->

## How it was tested

<!-- Commands you ran and their result. New behaviour needs a test that fails without it. -->

## Checklist

- [ ] Commits are signed off (`git commit -s`)
- [ ] `go build ./... && go vet ./... && go test ./...` passes
- [ ] Dashboard changes: `npx vitest run` and `npx tsc --noEmit` pass
- [ ] New migrations follow the rules in `CONTRIBUTING.md` (guards, `ON DELETE`, `.down.sql`)
- [ ] Dependency changes: `python scripts/third_party_notices.py --check` passes
