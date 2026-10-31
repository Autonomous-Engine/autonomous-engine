# Parse `<xmp>` contents as raw text

Repository: fb55/htmlparser2 (TypeScript).

## Request

The HTML standard defines `<xmp>` as a *raw text* element: everything up to the matching
`</xmp>` end tag is text, markup inside it is not parsed and character references are not
decoded (the same model as `<script>` and `<style>`). htmlparser2 currently parses the
contents of `<xmp>` as ordinary markup, so `<xmp><b>bold</b></xmp>` produces a `<b>` element
instead of the literal text `<b>bold</b>`.

## Acceptance criteria

- In HTML mode (the default), after an `<xmp>` start tag the tokenizer emits everything up to
  the next `</xmp>` end tag as text:
  - tags inside are not reported as tags (`onopentag`/`onclosetag` are not called for them),
    and the DOM (`parseDocument`) contains the `xmp` element with a single text child;
  - character references inside are left as written, even with `decodeEntities: true`;
  - the end tag is matched case-insensitively (`</XmP>` closes it), and only the full name:
    `</xmpx>` or `</x>` inside the element are text.
- Content after `</xmp>` is parsed normally.
- Other elements, including the existing special elements (`script`, `style`, `title`,
  `textarea`) and custom elements whose names start with `x`, behave exactly as before.
- In `xmlMode`, `<xmp>` is an ordinary element.

## Notes

- Special (raw text / RCDATA) elements are handled in `src/Tokenizer.ts`.
- Run the tests with `npx vitest run src`; existing snapshot tests must keep passing (add
  new snapshots if you add new snapshot tests).
