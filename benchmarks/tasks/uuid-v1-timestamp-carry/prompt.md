# v1/v6 timestamps jump ~430 s backwards when `nsecs` overflows the low 32 bits

Repository: uuidjs/uuid (TypeScript).

## Problem

A version 1 (and version 6) UUID carries a 60-bit timestamp: the number of 100-nanosecond
intervals since 1582-10-15, i.e. `(msecs + 12219292800000) * 10000 + nsecs`. `v1()` encodes
it as a 32-bit `time_low` plus `time_mid`/`time_high`, computing the two halves separately.
When adding `nsecs` pushes the low half past 2^32, the low half wraps but the high half is
not incremented, so the encoded timestamp is 2^32 × 100 ns ≈ 429.5 s too small.

This happens for one millisecond roughly every 430 seconds, for the `nsecs` values above the
boundary. It is reachable without options, because `v1()`'s internal state counts `nsecs` up
within a millisecond: ids generated in that millisecond go backwards in time, `v6` ids stop
sorting, and the same id can be produced twice 429.5 s apart.

## Expected behaviour

- For any `msecs` and any `nsecs` in `[0, 9999]`, the timestamp encoded by
  `v1({ msecs, nsecs, ... })` is exactly `(msecs + 12219292800000) * 10000 + nsecs`,
  including across the 32-bit carry. The same holds for `v6()`, which reorders the same
  timestamp.
- `v6` ids generated with increasing `nsecs` within one millisecond (and then the next
  millisecond) sort in generation order.
- All other output is unchanged, e.g. the RFC 9562 example
  `v1({ msecs: 0x17f22e279b0, nsecs: 0, clockseq: 0x33c8, node: [0x9f, 0x68, 0xde, 0xce, 0xd8, 0x46] })`
  is still `c232ab00-9414-11ec-b3c8-9f68deced846`.

## Notes

- The encoding is in `v1Bytes()` in `src/v1.ts`. JavaScript numbers cannot hold the full
  timestamp exactly, so keep the split-halves approach (or use `BigInt`) but carry correctly.
- Build and test with
  `npx tsc -p tsconfig.json --outDir dist-node && node --test --enable-source-maps 'dist-node/test/*.js'`.
