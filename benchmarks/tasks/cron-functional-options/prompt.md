# Refactor: functional options for cron.New

Repository: robfig/cron (Go), module `github.com/robfig/cron/v3`.

## Motivation

`Cron` is configured through a growing set of constructors and exported fields: `New()`,
`NewWithLocation(loc)`, the exported `ErrorLog` field, and a package-level default parser
(`Parse`) that every instance is hard-wired to. Adding the next setting (a per-instance
parser) would need yet another constructor. Move `Cron` to the functional-options pattern.

## Target API

In a new file `option.go`:

- `type Option func(*Cron)`
- `func WithLocation(loc *time.Location) Option` sets the time zone in which schedules are
  interpreted (default `time.Local`).
- `func WithParser(p Parser) Option` sets the `Parser` used by `AddFunc`/`AddJob` for this
  instance. The default is the standard 5-field parser with descriptors (the one behind
  `ParseStandard`).
- `func WithPanicLogger(l *log.Logger) Option` sets the logger used for job panics (default: a
  logger writing to `os.Stderr`).

In `cron.go`:

- `func New(opts ...Option) *Cron` applies the options in order over those defaults.
- `AddFunc`/`AddJob` parse specs with the instance's parser, not the package-level `Parse`.
- The exported `ErrorLog` field is replaced by the private logger set through
  `WithPanicLogger`.
- `NewWithLocation` is superseded: remove it, or keep it only as a thin deprecated wrapper over
  `New(WithLocation(loc))`.

In `parser.go`:

- Add `SecondOptional` to `ParseOption` so a parser can accept an optional leading seconds
  field. A `Parser` may carry at most one optional field (`SecondOptional` or `DowOptional`);
  `NewParser` panics if both are requested.
- Because every instance now carries its own parser, drop the package-level default parser
  that required a seconds field. `New()` with no options must reject a 6-field spec (5 fields
  expected). Callers that want seconds pass
  `WithParser(NewParser(Second | Minute | Hour | Dom | Month | Dow | Descriptor))`.

## Constraints

- Update the existing tests, `doc.go` and the README examples to the new API; the existing
  tests must keep passing.
- Do not change scheduling semantics (`Schedule`, `SpecSchedule.Next`, descriptors).
- `go test ./...` must pass.
