# Linkify keeps trailing punctuation when the URL is wrapped in parentheses

Repository: yuin/goldmark (Go), module `github.com/yuin/goldmark/v2`, package `extension`
(the Linkify extension).

## Problem

The Linkify extension turns bare URLs into autolinks and is meant to follow the GFM
autolink extension rules (as implemented by cmark-gfm) for where an autolink ends:
trailing punctuation (`?`, `!`, `.`, `,`, `:`, `*`, `_`, `~`) is not part of the link, an
unmatched trailing `)` is not part of the link, and a trailing entity-like `&name;` is not
part of the link.

Each rule works on its own, but they do not combine. When a URL sits inside parentheses and
ends with punctuation, the punctuation is kept in the link:

```
input:  see (https://example.com/docs.) now
got:    <p>see (<a href="https://example.com/docs.">https://example.com/docs.</a>) now</p>
want:   <p>see (<a href="https://example.com/docs">https://example.com/docs</a>.) now</p>
```

The same happens for `www.` autolinks, for the other trailing punctuation characters, and when
the URL itself contains a balanced pair of parentheses before the punctuation
(`(https://example.com/x_(y).)` must link `https://example.com/x_(y)`).

## Expected behaviour

- The trailing-character trimming is applied repeatedly until the end of the link no longer
  changes: removing an unmatched `)` can expose punctuation (or an entity-like suffix) that
  must then be removed too, and vice versa.
- A `)` is only removed while the link contains more `)` than `(`; balanced parentheses stay
  in the link.
- Existing behaviour is unchanged for links that do not need repeated trimming: a single
  trailing punctuation character, a single unmatched `)`, balanced parentheses inside the
  URL, query strings with `&`, and a trailing `&name;`.

## Notes

- The relevant code is the linkify inline parser in `extension/linkify.go`.
- You may add cases to the extension's existing test data.
- `go test ./...` must pass.
