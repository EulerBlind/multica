import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import {
  PLATFORM_NAV_ICON_MAP,
  getPlatformNavIconDefinition,
} from "./platform-nav-icon-map";

const require = createRequire(import.meta.url);
const ionicons = require("@expo/vector-icons/build/vendor/react-native-vector-icons/glyphmaps/Ionicons.json") as Record<
  string,
  number
>;

describe("platform navigation icon map", () => {
  it("maps every logical Android icon to a bundled Ionicons glyph", () => {
    for (const mapping of Object.values(PLATFORM_NAV_ICON_MAP)) {
      expect(ionicons[mapping.android.unfocused]).toBeTypeOf("number");
      expect(ionicons[mapping.android.focused]).toBeTypeOf("number");
    }
  });

  it("provides focused and unfocused iOS symbols and a safe runtime fallback", () => {
    for (const name of Object.keys(PLATFORM_NAV_ICON_MAP)) {
      expect(getPlatformNavIconDefinition(name, "ios", false)).not.toBe("");
      expect(getPlatformNavIconDefinition(name, "ios", true)).not.toBe("");
    }
    expect(getPlatformNavIconDefinition("unknown", "android")).toBe("help-circle-outline");
    expect(getPlatformNavIconDefinition("unknown", "ios", true)).toBe("questionmark.circle.fill");
  });

  it("keeps business callers free from direct sf: sources", () => {
    const directory = path.dirname(fileURLToPath(import.meta.url));
    const callers = [
      path.resolve(directory, "../../app/(app)/[workspace]/(tabs)/_layout.tsx"),
      path.resolve(directory, "./more-tab-dropdown.tsx"),
      path.resolve(directory, "../../app/(app)/[workspace]/switch-workspace.tsx"),
    ];
    for (const caller of callers) {
      expect(fs.readFileSync(caller, "utf8"), caller).not.toContain("sf:");
    }
  });
});
