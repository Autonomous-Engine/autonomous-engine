import { colord, extend } from "../src/";
import hwbPlugin from "../src/plugins/hwb";
import lchPlugin from "../src/plugins/lch";

extend([hwbPlugin, lchPlugin]);

// Held-out acceptance tests: every reported hue lies in [0, 360).

const nearRed = [
  { r: 255, g: 0, b: 2 },
  { r: 200, g: 10, b: 11 },
  { r: 250, g: 100, b: 101 },
  { r: 180, g: 40, b: 41 },
];

describe("bench: hue stays within [0, 360)", () => {
  it("HSL and HSV report 0 instead of 360", () => {
    for (const rgb of nearRed) {
      const color = colord(rgb);
      expect(color.toHsl().h).toBe(0);
      expect(color.toHsv().h).toBe(0);
      expect(color.hue()).toBe(0);
      expect(color.toHslString()).toMatch(/^hsl\(0, /);
    }
  });

  it("HSL strings and HSV objects round-trip unchanged", () => {
    for (const rgb of nearRed) {
      const color = colord(rgb);
      const hsl = color.toHslString();
      expect(colord(hsl).toHslString()).toBe(hsl);
      const hsv = color.toHsv();
      expect(colord(hsv).toHsv()).toEqual(hsv);
    }
  });

  it("HWB uses the same wrapped hue", () => {
    const color = colord({ r: 250, g: 100, b: 101 });
    expect(color.toHwb().h).toBe(0);
    expect(color.toHwbString()).toBe("hwb(0 39% 2%)");
    expect(color.toHwb().h).toBe(color.toHsl().h);
  });

  it("LCH hue wraps to 0 as well", () => {
    const color = colord({ r: 54, g: 4, b: 26 });
    expect(color.toLch().h).toBe(0);
    expect(color.toLchString()).toBe("lch(8.74% 26.07 0)");
  });

  it("rotating across the boundary yields hues in range", () => {
    const base = colord({ r: 200, g: 10, b: 11 });
    expect(base.rotate(30).hue()).toBe(30);
    expect(base.rotate(-30).hue()).toBe(330);
    expect(colord("hsl(340, 80%, 40%)").rotate(20).hue()).toBe(0);
  });

  it("every accessor agrees and stays below 360 across the red wheel", () => {
    for (let b = 0; b <= 6; b++) {
      for (const r of [120, 180, 255]) {
        const color = colord({ r, g: 0, b });
        const hue = color.hue();
        expect(hue).toBeGreaterThanOrEqual(0);
        expect(hue).toBeLessThan(360);
        expect(color.toHsl().h).toBe(hue);
        expect(color.toHsv().h).toBe(hue);
        expect(color.toHwb().h).toBe(hue);
      }
    }
  });

  it("a hue of 360 on input is the same color as 0", () => {
    expect(colord("hsl(360, 70%, 45%)").toHex()).toBe(colord("hsl(0, 70%, 45%)").toHex());
    expect(colord({ h: 360, s: 70, l: 45 }).toHsl().h).toBe(0);
  });
});
