# Nth-weekday (`#`) day of week suppresses a restricted day of month

Repository: harrisiirak/cron-parser (TypeScript).

## Problem

In cron, when both the day-of-month and the day-of-week fields are restricted (neither is
`*`), a day matches if it satisfies *either* field. cron-parser implements this OR rule for
ordinary weekdays, but not when the day of week uses the nth-occurrence form `weekday#n`.

For example, `0 0 8 * 5#3` should fire at midnight on the 8th of every month **and** on the
third Friday of every month. Today the 8th is skipped unless it happens to be the third
Friday, so the expression behaves like a bare `0 0 * * 5#3`. `CronExpression#includesDate`
has the same problem.

## Expected behaviour

- With a restricted day of month and a `weekday#n` day of week, a day matches when its day of
  month is listed **or** it is the nth occurrence of that weekday in its month.
- This holds for `next()`, `prev()`, iteration with `currentDate`/`endDate`, and
  `includesDate()`, with or without a seconds field and with month restrictions.
- `weekday#n` with a wildcard day of month still matches only the nth occurrence.
- Ordinary weekday/day-of-month combinations, `L` forms and all other fields behave as
  before.

## Notes

- The matching logic lives in `src/CronExpression.ts` (`#findSchedule`, the day-matching
  helpers and `includesDate`).
- Run the tests with `TZ=UTC npx jest`; the existing suites must keep passing.
