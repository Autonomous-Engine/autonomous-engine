# Hue can be reported as 360 instead of 0

Repository: omgovich/colord (TypeScript).

## Problem

Hues are angles in the range [0, 360), but colord sometimes reports a hue of exactly `360`.
It happens for reddish colours whose raw hue is just below 360 (for example
`rgb(255, 0, 2)`, raw hue about 359.5): the value is rounded *after* conversion and the
rounding produces 360. The result leaks into every API that exposes a hue:

```ts
colord({ r: 255, g: 0, b: 2 }).toHslString(); // "hsl(360, 100%, 50%)"
colord({ r: 255, g: 0, b: 2 }).hue();         // 360
```

Parsing then normalises 360 back to 0, so values do not round-trip
(`colord(c.toHslString()).toHslString() !== c.toHslString()`), and different accessors of the
same colour can disagree. The same problem exists for the HSV model, the `hwb` plugin
(`toHwb`, `toHwbString`) and the `lch` plugin (`toLch`, `toLchString`, which rounds the hue to
two decimals).

## Expected behaviour

- Every hue that colord reports — `hue()`, `toHsl()`, `toHslString()`, `toHsv()`, and the
  `hwb` and `lch` plugin outputs — is in [0, 360); a value that rounds to 360 is reported
  as 0.
- HSL strings and HSV objects produced by colord round-trip unchanged through `colord(...)`.
- `hue()`, `toHsl().h`, `toHsv().h` and `toHwb().h` agree for the same colour.
- A hue of 360 on input is still accepted and denotes the same colour as 0.
- All other conversions and roundings are unchanged.

## Notes

- Hue clamping and rounding helpers live in `src/helpers.ts`; the colour models are under
  `src/colorModels/`.
- Run the tests with `npx jest tests`; the existing suites must keep passing.
