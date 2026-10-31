# Add a `brand` plugin that snaps colours to the Brand Token palette

Repository: omgovich/colord (TypeScript).

## Request

Design wants colord to understand our brand palette so that product teams stop hard-coding
hex values. Add a new plugin, `brand`, modelled on the existing `names` plugin:

- `colord(x).toBrand()` returns the name of the nearest token in the **Brand Token
  specification BT-12** (the "Core" and "Extended" tables), using the perceptual distance and
  tie-breaking rules from section 5 of that specification;
- `colord("brand(<token>)")` parses a token reference, including the tint/shade suffixes
  (`-10` … `-90`) defined in section 3;
- tokens that the specification marks as *deprecated* must still parse but must never be
  returned by `toBrand()`.

Cover every token of both tables with tests, and document the plugin in the README like the
other plugins.

## Notes

- BT-12 is maintained by the design-system team outside this repository; its tables and the
  section 3/5 rules define the required values and behaviour.
- `npx jest tests` must pass.
