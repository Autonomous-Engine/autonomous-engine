import * as cssWhat from ".";

// Held-out acceptance tests for the SelectorType / AttributeAction enums. Reached through a
// loosely typed handle so this file compiles before the enums exist.
const api: any = cssWhat;

describe("bench: selector enums", () => {
    it("exports SelectorType with the existing string values", () => {
        expect(api.SelectorType).toBeDefined();
        expect({ ...api.SelectorType }).toEqual(
            expect.objectContaining({
                Attribute: "attribute",
                Pseudo: "pseudo",
                PseudoElement: "pseudo-element",
                Tag: "tag",
                Universal: "universal",
                Adjacent: "adjacent",
                Child: "child",
                Descendant: "descendant",
                Parent: "parent",
                Sibling: "sibling",
            })
        );
    });

    it("exports AttributeAction with the existing string values", () => {
        expect(api.AttributeAction).toBeDefined();
        expect({ ...api.AttributeAction }).toEqual(
            expect.objectContaining({
                Any: "any",
                Element: "element",
                End: "end",
                Equals: "equals",
                Exists: "exists",
                Hyphen: "hyphen",
                Not: "not",
                Start: "start",
            })
        );
    });

    it("parse() produces enum members", () => {
        const { SelectorType, AttributeAction } = api;
        const [selector] = cssWhat.parse('div > a[href^="http"]:not(.x) ~ *::before');
        expect(selector.map((s) => s.type)).toEqual([
            SelectorType.Tag,
            SelectorType.Child,
            SelectorType.Tag,
            SelectorType.Attribute,
            SelectorType.Pseudo,
            SelectorType.Sibling,
            SelectorType.Universal,
            SelectorType.PseudoElement,
        ]);
        const attr: any = selector[3];
        expect(attr.action).toBe(AttributeAction.Start);
        const [[cls]] = (selector[4] as any).data;
        expect(cls.action).toBe(AttributeAction.Element);
    });

    it("stringify() accepts selectors built from the enums", () => {
        const { SelectorType, AttributeAction } = api;
        const built = [
            [
                { type: SelectorType.Tag, name: "ul", namespace: null },
                { type: SelectorType.Descendant },
                {
                    type: SelectorType.Attribute,
                    name: "data-v",
                    action: AttributeAction.Hyphen,
                    value: "en",
                    namespace: null,
                    ignoreCase: false,
                },
                { type: SelectorType.Adjacent },
                { type: SelectorType.Universal, namespace: null },
            ],
        ];
        const text = cssWhat.stringify(built as any);
        expect(cssWhat.parse(text)).toEqual(built);
    });

    it("parse/stringify round-trips are unchanged", () => {
        for (const selector of [
            "a.b#c",
            "div:has(> p)",
            "[data-x$=\"y\" i]",
            "ns|tag",
            "*|*",
            "a , b > c + d ~ e",
            "::selection",
            ":nth-child(2n+1)",
        ]) {
            const parsed = cssWhat.parse(selector);
            expect(cssWhat.parse(cssWhat.stringify(parsed))).toEqual(parsed);
        }
        expect(cssWhat.isTraversal({ type: api.SelectorType.Parent } as any)).toBe(true);
        expect(cssWhat.isTraversal({ type: api.SelectorType.Tag, name: "a", namespace: null } as any)).toBe(false);
    });
});
