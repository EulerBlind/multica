import { describe, expect, it } from "vitest";
import { buildTimelineRows } from "./timeline-thread";
import type { TimelineEntry } from "@multica/core/types";

function comment(partial: Partial<TimelineEntry>): TimelineEntry {
  return {
    type: "comment",
    id: "c-" + Math.random().toString(36).slice(2, 8),
    actor_type: "member",
    actor_id: "u-1",
    content: "",
    parent_id: null,
    created_at: "2026-01-01T00:00:00Z",
    ...partial,
  };
}

describe("buildTimelineRows", () => {
  it("keeps top-level comments as rows and folds replies into their parent", () => {
    const root = comment({ id: "root-1", content: "root" });
    const reply = comment({ id: "reply-1", parent_id: "root-1", content: "reply" });
    const independent = comment({ id: "root-2", content: "independent" });

    const rows = buildTimelineRows([root, reply, independent]);

    expect(rows).toHaveLength(2);
    expect(rows[0].entry.id).toBe("root-1");
    expect(rows[0].replies.map((r) => r.id)).toEqual(["reply-1"]);
    expect(rows[1].entry.id).toBe("root-2");
    expect(rows[1].replies).toEqual([]);
  });

  it("flattens nested reply-to-reply chains into the same bundle (A → B → C)", () => {
    const a = comment({ id: "a", content: "A" });
    const b = comment({ id: "b", parent_id: "a", content: "B" });
    const c = comment({ id: "c", parent_id: "b", content: "C" });

    const rows = buildTimelineRows([a, b, c]);

    expect(rows).toHaveLength(1);
    expect(rows[0].entry.id).toBe("a");
    expect(rows[0].replies.map((r) => r.id)).toEqual(["b", "c"]);
  });

  it("promotes orphan replies (parent not in the batch) to top level", () => {
    const orphan = comment({ id: "orphan", parent_id: "missing-parent", content: "orphan" });
    const independent = comment({ id: "root", content: "root" });

    const rows = buildTimelineRows([orphan, independent]);

    expect(rows).toHaveLength(2);
    expect(rows.map((r) => r.entry.id).sort()).toEqual(["orphan", "root"]);
    expect(rows.find((r) => r.entry.id === "orphan")?.replies).toEqual([]);
  });

  it("emits the same total entry count (counts must agree with web)", () => {
    const root = comment({ id: "root", content: "root" });
    const reply = comment({ id: "reply", parent_id: "root", content: "reply" });
    const activity = {
      type: "activity" as const,
      id: "act-1",
      actor_type: "agent",
      actor_id: "a-1",
      action: "task_failed",
      created_at: "2026-01-01T00:00:00Z",
    };

    const rows = buildTimelineRows([root, reply, activity]);
    const emitted = rows.reduce((n, r) => n + 1 + r.replies.length, 0);

    expect(emitted).toBe(3);
  });
});
