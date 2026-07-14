import { describe, expect, it } from "vitest";
import { normalizeClientOS } from "./client-os";

describe("client OS identity", () => {
  it("preserves supported native platforms and does not impersonate iOS", () => {
    expect(normalizeClientOS("android")).toBe("android");
    expect(normalizeClientOS("ios")).toBe("ios");
    expect(normalizeClientOS("web")).toBe("unknown");
  });
});
