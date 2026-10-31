import { execSync } from "node:child_process";
import { describe, expect, it } from "vitest";
import { match, pathToRegexp } from "./index.js";

// Held-out acceptance tests: the package is compiled for ES2015 and generates regular
// expressions that ES2015 engines understand (no ES2018 `s`/dotAll flag).

const root = process.cwd();

function tsc(args: string): { code: number; out: string } {
  try {
    const out = execSync(`npx tsc ${args}`, {
      cwd: root,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    });
    return { code: 0, out };
  } catch (err: any) {
    return { code: err.status ?? 1, out: String(err.stdout ?? "") };
  }
}

describe("bench: ES2015 compatibility", () => {
  it("compiles with an ES2015 target and library", () => {
    const { code, out } = tsc("--showConfig -p tsconfig.json");
    expect(code).toBe(0);
    const config = JSON.parse(out).compilerOptions;
    // tsc --showConfig spells ES2015 as "es6"
    const es2015 = (value: string) => (value.toLowerCase() === "es6" ? "es2015" : value.toLowerCase());
    expect(es2015(String(config.target))).toBe("es2015");
    expect((config.lib as string[]).map(es2015)).toEqual(["es2015"]);
  });

  it("type-checks against the ES2015 library", () => {
    const { code, out } = tsc("--noEmit -p tsconfig.json");
    expect(out.match(/error TS\d+/g) ?? []).toEqual([]);
    expect(code).toBe(0);
  }, 60_000);

  it("generated regexps do not use the dotAll flag", () => {
    expect(pathToRegexp("/users/:id").regexp.flags).toBe("i");
    expect(pathToRegexp("/users/:id", { sensitive: true }).regexp.flags).toBe("");
    expect(pathToRegexp(["/a/*rest", "/b/:x"]).regexp.flags).not.toContain("s");
  });

  it("wildcards still match line terminators", () => {
    const fn = match("/files/*rest");
    const result = fn("/files/a\nb/c\rd");
    expect(result).not.toBe(false);
    expect(result && result.params.rest).toEqual(["a\nb", "c\rd"]);
  });

  it("matching behaviour is otherwise unchanged", () => {
    expect(match("/users/:id")("/USERS/42")).toEqual({
      path: "/USERS/42",
      params: { id: "42" },
    });
    expect(match("/users/:id", { sensitive: true })("/USERS/42")).toBe(false);
    expect(match("/a{/:b}")("/a")).toEqual({ path: "/a", params: {} });
  });
});
