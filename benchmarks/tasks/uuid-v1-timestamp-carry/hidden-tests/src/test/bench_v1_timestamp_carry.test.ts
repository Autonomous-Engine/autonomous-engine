import * as assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import { v1, v6 } from '../index.js';

// Held-out acceptance tests: the 60-bit v1/v6 timestamp equals
// (msecs + Gregorian offset) * 10000 + nsecs exactly, including when nsecs carries
// out of the low 32 bits.

const GREGORIAN_OFFSET_MS = 12219292800000n;
const NODE = Uint8Array.of(0x01, 0x23, 0x45, 0x67, 0x89, 0xab);
const CLOCKSEQ = 0x1234;

function expectedTimestamp(msecs: number, nsecs: number): bigint {
  return (BigInt(msecs) + GREGORIAN_OFFSET_MS) * 10000n + BigInt(nsecs);
}

function v1Timestamp(id: string): bigint {
  const [low, mid, hiAndVersion] = id.split('-');
  return (BigInt(`0x${hiAndVersion}`) & 0xfffn) << 48n | BigInt(`0x${mid}`) << 32n | BigInt(`0x${low}`);
}

function v6Timestamp(id: string): bigint {
  const hex = id.replace(/-/g, '');
  // v6: 48 high bits, version nibble, 12 low bits
  return (BigInt(`0x${hex.slice(0, 12)}`) << 12n) | BigInt(`0x${hex.slice(13, 16)}`);
}

// Find millisecond values whose low 32 timestamp bits overflow once nsecs is added.
function boundaryCases(count: number): { msecs: number; nsecs: number }[] {
  const out: { msecs: number; nsecs: number }[] = [];
  for (let msecs = 1_700_000_000_000; out.length < count; msecs++) {
    const low = (expectedTimestamp(msecs, 0) & 0xffffffffn);
    const room = 0x100000000n - low;
    if (room <= 9999n) {
      out.push({ msecs, nsecs: Number(room) }); // exactly at the carry
      out.push({ msecs, nsecs: 9999 }); // well past the carry
    }
  }
  return out;
}

describe('bench: v1/v6 timestamp carry', () => {
  const cases = boundaryCases(4);

  test('v1 encodes the exact timestamp across a 32-bit carry', () => {
    for (const { msecs, nsecs } of cases) {
      const id = v1({ msecs, nsecs, clockseq: CLOCKSEQ, node: NODE });
      assert.equal(v1Timestamp(id), expectedTimestamp(msecs, nsecs), `msecs=${msecs} nsecs=${nsecs} -> ${id}`);
    }
  });

  test('v6 encodes the exact timestamp across a 32-bit carry', () => {
    for (const { msecs, nsecs } of cases) {
      const id = v6({ msecs, nsecs, clockseq: CLOCKSEQ, node: NODE });
      assert.equal(v6Timestamp(id), expectedTimestamp(msecs, nsecs), `msecs=${msecs} nsecs=${nsecs} -> ${id}`);
    }
  });

  test('v6 ids stay sorted through the carry within one millisecond', () => {
    const { msecs } = cases[0];
    const ids: string[] = [];
    for (let nsecs = 0; nsecs < 10000; nsecs += 250) {
      ids.push(v6({ msecs, nsecs, clockseq: CLOCKSEQ, node: NODE }));
    }
    ids.push(v6({ msecs: msecs + 1, nsecs: 0, clockseq: CLOCKSEQ, node: NODE }));
    assert.deepEqual([...ids].sort(), ids);
  });

  test('ordinary timestamps are unchanged', () => {
    for (const [msecs, nsecs] of [[0, 0], [1321644961388, 0], [1321644961388, 5000], [1700000000000, 1]] as const) {
      const id = v1({ msecs, nsecs, clockseq: CLOCKSEQ, node: NODE });
      assert.equal(v1Timestamp(id), expectedTimestamp(msecs, nsecs));
    }
    assert.equal(
      v1({ msecs: 0x17f22e279b0, nsecs: 0, clockseq: 0x33c8, node: Uint8Array.of(0x9f, 0x68, 0xde, 0xce, 0xd8, 0x46) }),
      'c232ab00-9414-11ec-b3c8-9f68deced846',
    );
  });
});
