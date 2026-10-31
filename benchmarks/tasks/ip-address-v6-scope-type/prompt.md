# `Address6#getScope()` is wrong for unicast addresses; `getType()` misses common prefixes

Repository: beaugunderson/ip-address (TypeScript).

## Problem

`Address6#getScope()` reads the 4-bit scope field (bits 12-16) for every address. That field
only exists in multicast addresses (RFC 4291 §2.7), so unicast results are nonsense:
`fe80::1` is reported as `'Reserved'` and `2002::1` as `'Link local'`.

`Address6#getType()` also does not recognise several well-known prefixes (they all come back
as `'Global unicast'`), and `isMulticast()` returns `false` for well-known multicast groups
such as `ff01::1`, whose type string is `'Multicast (All nodes on this interface)'`.

## Expected behaviour

`getScope()`:

- Multicast addresses (any type that is `'Multicast'` or starts with `'Multicast '`) keep
  using the scope field and the existing `SCOPES` names; an undefined scope value gives
  `'Unknown'`.
- Unicast scope follows RFC 4007 §6: link-local unicast (`fe80::/10`) and the loopback
  address `::1` (RFC 4291 §2.5.3) are `'Link local'`; the unspecified address `::` is
  `'Unknown'`; every other unicast address (including unique-local and 6to4) is `'Global'`.

`getType()` additionally recognises, with exactly these names:

| prefix            | type                   |
|-------------------|------------------------|
| `fc00::/7`        | `'Unique local'`       |
| `2002::/16`       | `'6to4'`               |
| `2001:db8::/32`   | `'Documentation'`      |
| `64:ff9b::/96`    | `'NAT64 (well-known)'` |
| `64:ff9b:1::/48`  | `'NAT64 (local-use)'`  |

Existing classifications are unchanged.

`isMulticast()` returns `true` for every multicast address, including the named groups.

## Notes

- See `src/ipv6.ts` and the tables in `src/v6/constants.ts`.
- Run the tests with `npx mocha` (configuration in `.mocharc.yml`); existing tests must
  keep passing.
