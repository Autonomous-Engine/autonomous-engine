# Switch day-of-month / day-of-week combination to AND semantics

Repository: harrisiirak/cron-parser (TypeScript).

## Request

Our scheduling service is migrating jobs from a Quartz-based system. In those schedules a
restricted day of month together with a restricted day of week means *both* must match (for
example `0 0 13 * 5` = "every Friday the 13th"), whereas cron-parser follows Vixie cron and
treats the combination as *either* (the 13th of every month **or** every Friday).

Change cron-parser so that a restricted day of month and a restricted day of week are combined
with AND.

## Acceptance criteria

1. `0 0 13 * 5` fires only on Fridays that fall on the 13th; the same applies to `next()`,
   `prev()`, iteration and `includesDate()`.
2. The change must be fully backward compatible: no expression that existing users have
   stored may change its sequence of fire times, and the existing test suite must keep
   passing unchanged.
3. No new option or alternative syntax: the downstream services parse expressions with the
   default `CronExpressionParser.parse(expr)` call and cannot be changed in this release.

## Notes

- Day matching lives in `src/CronExpression.ts`.
- Run the tests with `TZ=UTC npx jest`.
