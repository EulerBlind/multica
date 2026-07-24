import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/**
 * Guard: Autopilots entry must stay in-app (router.push), never open the
 * web Autopilots page via Linking.openURL. QIA-319 replan rejected the
 * external deep-link approach.
 */
describe("more-tab Autopilots entry", () => {
  const moreTabPath = path.resolve(
    path.dirname(fileURLToPath(import.meta.url)),
    "./more-tab-dropdown.tsx",
  );
  const source = fs.readFileSync(moreTabPath, "utf8");

  it("routes Autopilots through in-app /more/autopilots", () => {
    expect(source).toContain('path: "/more/autopilots"');
    expect(source).toContain('label: "Autopilots"');
  });

  it("does not open Autopilots via Linking.openURL", () => {
    // No Linking import / call — only an explanatory comment may mention it.
    expect(source).not.toMatch(/from "react-native".*Linking|Linking,/);
    expect(source).not.toMatch(/Linking\.openURL\(/);
    expect(source).not.toMatch(/AUTOPILOTS_WEB_PATH/);
    expect(source).not.toMatch(/EXPO_PUBLIC_WEB_URL.*autopilots|autopilotsHref/);
  });
});
