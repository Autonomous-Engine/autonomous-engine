import { describe, expect, it } from "vitest";
import { DomUtils, parseDocument, Parser } from "./index.js";

// Held-out acceptance tests: <xmp> content is raw text, like <textarea>/<title> but without
// entity decoding (the HTML "raw text" model shared with <script>/<style>).

function events(html: string, options = {}) {
    const log: string[] = [];
    const parser = new Parser(
        {
            onopentag: (name) => log.push(`open:${name}`),
            onclosetag: (name) => log.push(`close:${name}`),
            ontext: (text) => log.push(`text:${text}`),
        },
        options,
    );
    parser.write(html);
    parser.end();
    // merge adjacent text events
    const merged: string[] = [];
    for (const entry of log) {
        const last = merged[merged.length - 1];
        if (entry.startsWith("text:") && last?.startsWith("text:")) {
            merged[merged.length - 1] = last + entry.slice(5);
        } else {
            merged.push(entry);
        }
    }
    return merged;
}

describe("bench: xmp raw text", () => {
    it("does not parse markup inside <xmp>", () => {
        expect(events("<xmp><b>bold</b> & <i>x</i></xmp>after")).toEqual([
            "open:xmp",
            "text:<b>bold</b> & <i>x</i>",
            "close:xmp",
            "text:after",
        ]);
    });

    it("builds a DOM with a single text child", () => {
        const doc = parseDocument("<div><xmp><p>not a paragraph</p></xmp></div>");
        const xmp = DomUtils.getElementsByTagName("xmp", doc)[0];
        expect(xmp).toBeDefined();
        expect(xmp.children).toHaveLength(1);
        expect(DomUtils.getText(xmp)).toBe("<p>not a paragraph</p>");
        expect(DomUtils.getElementsByTagName("p", doc)).toHaveLength(0);
    });

    it("keeps character references undecoded", () => {
        expect(events("<xmp>&amp; &lt;tag&gt;</xmp>", { decodeEntities: true })).toEqual([
            "open:xmp",
            "text:&amp; &lt;tag&gt;",
            "close:xmp",
        ]);
    });

    it("closes on a case-insensitive end tag only", () => {
        expect(events("<XMP></xmpx></x></XmP>tail")).toEqual([
            "open:xmp",
            "text:</xmpx></x>",
            "close:xmp",
            "text:tail",
        ]);
    });

    it("leaves other special elements unchanged", () => {
        expect(events("<textarea><b>t</b></textarea><title><i>x</i></title>")).toEqual([
            "open:textarea",
            "text:<b>t</b>",
            "close:textarea",
            "open:title",
            "text:<i>x</i>",
            "close:title",
        ]);
        expect(events("<p><x-element>ok</x-element></p>")).toEqual([
            "open:p",
            "open:x-element",
            "text:ok",
            "close:x-element",
            "close:p",
        ]);
    });

    it("treats <xmp> as ordinary markup in XML mode", () => {
        expect(events("<xmp><b>x</b></xmp>", { xmlMode: true })).toEqual([
            "open:xmp",
            "open:b",
            "text:x",
            "close:b",
            "close:xmp",
        ]);
    });
});
