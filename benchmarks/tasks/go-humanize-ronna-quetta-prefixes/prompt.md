# Feature: support the 2022 SI prefixes ronna/quetta and ronto/quecto

Repository: dustin/go-humanize (Go).

## Request

The 27th CGPM (2022) added four SI prefixes: ronna (`R`, 10^27), quetta (`Q`, 10^30),
ronto (`r`, 10^-27) and quecto (`q`, 10^-30). The package stops at yotta/yocto, so values in
those magnitudes are formatted without a prefix and `ParseSI("2.2 RF")` fails. Add the new
prefixes everywhere the package exposes SI or IEC scaling.

## Acceptance criteria

1. `ComputeSI`, `SI` and `SIWithDigits` use `R`, `Q`, `r`, `q` for magnitudes 10^27, 10^30,
   10^-27 and 10^-30 (for example `SI(2.2e27, "F") == "2.2 RF"` and `SI(1e-30, "F") == "1 qF"`).
2. `ParseSI` accepts the new prefixes and returns the corresponding magnitude
   (for example `ParseSI("2.2 RF")` is approximately `2.2e27` with unit `F`).
3. Big byte sizes: exported `*big.Int` values `BigRByte` and `BigQByte` (decimal, 1000^9 and
   1000^10) and `BigRiByte` and `BigQiByte` (binary, 1024^9 and 1024^10) exist, following the
   naming of the existing `BigYByte`/`BigYiByte`.
4. `BigBytes` and `BigIBytes` format values in those ranges with the `RB`/`QB` and `RiB`/`QiB`
   suffixes using the existing number formatting (for example `BigBytes(BigRByte) == "1.0 RB"`
   and `BigIBytes(BigQiByte) == "1.0 QiB"`). Values below 10^27 / 1024^9 format exactly as
   before.
5. `ParseBigBytes` accepts the new suffixes, case-insensitively, the same way it accepts
   `YB`/`YiB` today.
6. The existing test that formats values above 1000 YB / 1024 YiB currently expects
   `16093 YB` and `13312 YiB`; those inputs now render with the new prefixes (`16 RB`,
   `13 RiB`), so update that expectation. All other existing tests pass unchanged.

## Notes

- `go test ./...` must pass.
