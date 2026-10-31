# Replace selector type and attribute action string unions with exported enums

Repository: fb55/css-what (TypeScript).

## Request

Selector `type`s (`"tag"`, `"attribute"`, `"child"`, ...) and attribute `action`s
(`"equals"`, `"start"`, ...) are spelled as string literals throughout the parser, the
stringifier and the type definitions. Consumers have no runtime constant to compare against
and typos are easy. Introduce two string enums, export them from the package entry point, and
use them everywhere instead of the bare literals.

## Acceptance criteria

- `src/types.ts` exports `enum SelectorType` with members `Attribute = "attribute"`,
  `Pseudo = "pseudo"`, `PseudoElement = "pseudo-element"`, `Tag = "tag"`,
  `Universal = "universal"`, and the traversals `Adjacent = "adjacent"`, `Child = "child"`,
  `Descendant = "descendant"`, `Parent = "parent"`, `Sibling = "sibling"`.
- `src/types.ts` exports `enum AttributeAction` with members `Any = "any"`,
  `Element = "element"`, `End = "end"`, `Equals = "equals"`, `Exists = "exists"`,
  `Hyphen = "hyphen"`, `Not = "not"`, `Start = "start"`, replacing the current
  `AttributeAction` string-union type.
- Both are available from the package entry point (`src/index.ts`).
- The selector interfaces (`AttributeSelector`, `TagSelector`, ..., `TraversalType`) are typed
  with the enum members, and `parse.ts` / `stringify.ts` use the enums instead of string
  literals. The test fixtures may be updated to the enum members.
- The runtime output is unchanged: the enum values are exactly the strings used today, so
  `parse()` results, `stringify()` output and `isTraversal()` behave as before (selectors built
  from the enum members stringify and parse back to the same structure).

## Notes

- Run the tests with `npx jest`.
