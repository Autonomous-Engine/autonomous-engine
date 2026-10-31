import { describe, expect, it } from "vitest";
import {
  camelCase,
  capitalCase,
  constantCase,
  dotCase,
  kebabCase,
  noCase,
  pascalCase,
  pascalSnakeCase,
  pathCase,
  sentenceCase,
  snakeCase,
  trainCase,
} from "./index.js";

// Held-out acceptance tests for the `suffixCharacters` option.

describe("bench: suffixCharacters", () => {
  it("keeps configured trailing characters for every case function", () => {
    const options = { suffixCharacters: "_" };
    const input = "user_id__";
    expect(camelCase(input, options)).toBe("userId__");
    expect(pascalCase(input, options)).toBe("UserId__");
    expect(pascalSnakeCase(input, options)).toBe("User_Id__");
    expect(capitalCase(input, options)).toBe("User Id__");
    expect(constantCase(input, options)).toBe("USER_ID__");
    expect(dotCase(input, options)).toBe("user.id__");
    expect(kebabCase(input, options)).toBe("user-id__");
    expect(noCase(input, options)).toBe("user id__");
    expect(pathCase(input, options)).toBe("user/id__");
    expect(sentenceCase(input, options)).toBe("User id__");
    expect(snakeCase(input, options)).toBe("user_id__");
    expect(trainCase(input, options)).toBe("User-Id__");
  });

  it("accepts several suffix characters in any order", () => {
    expect(camelCase("my value$_", { suffixCharacters: "_$" })).toBe("myValue$_");
    expect(snakeCase("className$", { suffixCharacters: "$" })).toBe("class_name$");
  });

  it("combines with prefixCharacters", () => {
    const options = { prefixCharacters: "_", suffixCharacters: "_" };
    expect(camelCase("__private_field_", options)).toBe("__privateField_");
    expect(kebabCase("__private_field_", options)).toBe("__private-field_");
    expect(capitalCase("_x_", options)).toBe("_X_");
  });

  it("does not duplicate characters when the whole input is prefix/suffix characters", () => {
    expect(camelCase("___", { suffixCharacters: "_" })).toBe("___");
    expect(camelCase("___", { prefixCharacters: "_", suffixCharacters: "_" })).toBe("___");
  });

  it("only keeps characters at the end of the input", () => {
    expect(camelCase("foo$bar", { suffixCharacters: "$" })).toBe("fooBar");
    expect(snakeCase("foo_bar_", { suffixCharacters: "_" })).toBe("foo_bar_");
  });

  it("strips trailing characters when the option is not set", () => {
    expect(camelCase("type_")).toBe("type");
    expect(snakeCase("type__")).toBe("type");
    expect(camelCase("type_", { prefixCharacters: "_" })).toBe("type");
  });
});
