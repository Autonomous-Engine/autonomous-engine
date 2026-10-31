# `satisfies` mis-handles tilde/caret ranges with zero or missing segments and pre-releases

Repository: omichelsen/compare-versions (TypeScript).

## Problem

`satisfies(version, range)` is supposed to follow npm's semver rules for `~` and `^` ranges,
but it compares only the major version and then "the rest is greater or equal". That is wrong
in several cases:

- Leading-zero caret ranges: `^0.4.2` must not accept `0.5.0`, and `^0.0.7` must not accept
  `0.0.8` (npm: `^0.4.2` := `>=0.4.2 <0.5.0`, `^0.0.7` := `>=0.0.7 <0.0.8`).
- Partial tilde ranges: `~2.4` means `2.4.x` and `~2` means `2.x`, but versions such as
  `2.4.9` (for `~2.4`) or `2.0.0` (for `~2`) are currently rejected; `~0` must accept any `0.x.y`.
- Ranges with a pre-release (for example `~3.1.4-rc.1`) ignore the pre-release part, so
  `3.1.4-rc.0` or `3.1.5-rc.1` are accepted.

## Expected behaviour

- `~M.m.p` := `>=M.m.p <M.(m+1).0`; `~M.m` := `M.m.x`; `~M` := `M.x`.
- `^M.m.p` := `>=M.m.p` and below the next increment of the left-most non-zero segment
  (`^2.3.4` := `<3.0.0`, `^0.4.2` := `<0.5.0`, `^0.0.7` := `<0.0.8`); missing segments act as
  wildcards (`^2.0` := `2.x`).
- When the range carries a pre-release tag, it is satisfied only by a pre-release of the
  *same* `major.minor.patch` whose pre-release identifiers compare greater than or equal to
  the range's (identifiers are compared segment by segment, numerically where numeric, so
  `rc.10` > `rc.9`). A version without a pre-release does not satisfy such a range.
- A leading `v` on the version is still accepted; comparison operators (`>`, `>=`, `<`, `<=`,
  `=`), exact versions and `x`/`*` wildcard ranges behave as before.
- Out of scope: how a *version* with a pre-release relates to a range without one.

## Notes

- The implementation is `satisfies` in `src/index.ts`.
- Some of the existing expectations in `test/satisfies.ts` encode the old behaviour; update
  them where they contradict the rules above.
- Run the tests with `npx mocha -r ts-node/register 'test/**/*.ts'`.
