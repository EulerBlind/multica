import fs from "node:fs";
import path from "node:path";
import { describe, expect, it, vi } from "vitest";
import { getActionSheetIndexes, runActionItemOnce } from "./action-sheet";

describe("cross-platform action sheet contract", () => {
  it("maps cancel, destructive, and disabled actions to native indices", () => {
    const noop = () => {};
    expect(
      getActionSheetIndexes([
        { key: "edit", label: "Edit", onPress: noop },
        { key: "delete", label: "Delete", role: "destructive", onPress: noop },
        { key: "later", label: "Later", disabled: true, onPress: noop },
        { key: "cancel", label: "Cancel", role: "cancel", onPress: noop },
      ]),
    ).toEqual({
      cancelButtonIndex: 3,
      destructiveButtonIndex: 1,
      disabledButtonIndices: [2],
    });
  });

  it("fires a selected action at most once", () => {
    const onPress = vi.fn();
    const items = [{ key: "reply", label: "Reply", onPress }];
    let handled = runActionItemOnce(items, 0, false);
    handled = runActionItemOnce(items, 0, handled);
    expect(handled).toBe(true);
    expect(onPress).toHaveBeenCalledTimes(1);
  });

  it("keeps direct ActionSheetIOS calls inside the adapter", () => {
    const root = process.cwd();
    const sources = ["app", "components"];
    const offenders: string[] = [];

    const visit = (directory: string) => {
      for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
        const absolute = path.join(directory, entry.name);
        if (entry.isDirectory()) visit(absolute);
        else if (/\.tsx?$/.test(entry.name)) {
          const relative = path.relative(root, absolute);
          if (relative === "lib/action-sheet-ios.ts") continue;
          if (/ActionSheetIOS\.showActionSheetWithOptions/.test(fs.readFileSync(absolute, "utf8"))) {
            offenders.push(relative);
          }
        }
      }
    };

    sources.forEach((source) => visit(path.join(root, source)));
    expect(offenders).toEqual([]);
  });
});
