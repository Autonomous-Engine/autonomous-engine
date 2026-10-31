import { CronExpressionParser } from '../src';

// Held-out acceptance tests: a restricted day of month and an nth-weekday (`#`) day of week
// combine with OR, like every other restricted day-of-month / day-of-week pair.
// The oracle below enumerates 2026 day by day (tests run with TZ=UTC).

type DayRule = (d: Date) => boolean;

const nthOf = (d: Date) => Math.ceil(d.getUTCDate() / 7);

function oracle(rule: DayRule): string[] {
  const out: string[] = [];
  for (let t = Date.UTC(2026, 0, 1); t < Date.UTC(2027, 0, 1); t += 86400000) {
    const d = new Date(t);
    if (rule(d)) out.push(d.toISOString());
  }
  return out;
}

function schedule(expression: string): string[] {
  const it = CronExpressionParser.parse(expression, {
    currentDate: new Date('2025-12-31T12:00:00.000Z'),
    endDate: new Date('2026-12-31T23:59:59.000Z'),
  });
  const out: string[] = [];
  while (it.hasNext()) {
    out.push(it.next().toISOString() as string);
    if (out.length > 400) break;
  }
  return out;
}

describe('bench: nth weekday combined with day of month', () => {
  it('"0 0 8 * 5#3" fires on the 8th and on the third Friday', () => {
    const expected = oracle((d) => d.getUTCDate() === 8 || (d.getUTCDay() === 5 && nthOf(d) === 3));
    expect(schedule('0 0 8 * 5#3')).toEqual(expected);
  });

  it('a list of days of month combines with an nth weekday', () => {
    const expected = oracle(
      (d) => d.getUTCDate() === 1 || d.getUTCDate() === 15 || (d.getUTCDay() === 1 && nthOf(d) === 2),
    );
    expect(schedule('0 0 1,15 * 1#2')).toEqual(expected);
  });

  it('works with a seconds field and a month restriction', () => {
    const expected = oracle(
      (d) => [2, 5, 11].includes(d.getUTCMonth() + 1) && (d.getUTCDate() === 20 || (d.getUTCDay() === 0 && nthOf(d) === 1)),
    );
    expect(schedule('0 0 0 20 2,5,11 0#1')).toEqual(expected);
  });

  it('includesDate agrees with the schedule', () => {
    const expr = CronExpressionParser.parse('0 0 8 * 5#3');
    // 2026-06-08 is a Monday (the 8th); 2026-06-19 is the third Friday; 2026-06-12 is the second Friday.
    expect(expr.includesDate(new Date('2026-06-08T00:00:00.000Z'))).toBe(true);
    expect(expr.includesDate(new Date('2026-06-19T00:00:00.000Z'))).toBe(true);
    expect(expr.includesDate(new Date('2026-06-12T00:00:00.000Z'))).toBe(false);
    expect(expr.includesDate(new Date('2026-06-09T00:00:00.000Z'))).toBe(false);
  });

  it('an nth weekday with a wildcard day of month is unchanged', () => {
    const expected = oracle((d) => d.getUTCDay() === 2 && nthOf(d) === 4);
    expect(schedule('0 0 * * 2#4')).toEqual(expected);
  });

  it('a plain weekday with a day of month is unchanged', () => {
    const expected = oracle((d) => d.getUTCDate() === 13 || d.getUTCDay() === 5);
    expect(schedule('0 0 13 * 5')).toEqual(expected);
  });

  it('prev() walks the same OR schedule backwards', () => {
    const it = CronExpressionParser.parse('0 0 8 * 5#3', { currentDate: new Date('2026-07-01T00:00:00.000Z') });
    expect(it.prev().toISOString()).toBe('2026-06-19T00:00:00.000Z');
    expect(it.prev().toISOString()).toBe('2026-06-08T00:00:00.000Z');
    expect(it.prev().toISOString()).toBe('2026-05-15T00:00:00.000Z');
    expect(it.prev().toISOString()).toBe('2026-05-08T00:00:00.000Z');
  });
});
