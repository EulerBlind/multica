import { describe, expect, it } from "vitest";
import {
  autopilotExecutionModeLabel,
  autopilotRunStatusLabel,
  autopilotStatusLabel,
  autopilotTriggerKindLabel,
  formatTriggerKinds,
} from "./autopilot-labels";

describe("autopilot-labels", () => {
  it("maps known status / mode / run / trigger labels", () => {
    expect(autopilotStatusLabel("active")).toBe("Active");
    expect(autopilotStatusLabel("paused")).toBe("Paused");
    expect(autopilotExecutionModeLabel("create_issue")).toBe("Create issue");
    expect(autopilotExecutionModeLabel("run_only")).toBe("Run only");
    expect(autopilotRunStatusLabel("skipped")).toBe("Skipped");
    expect(autopilotTriggerKindLabel("schedule")).toBe("Schedule");
  });

  it("falls through unknown enum values instead of throwing", () => {
    expect(autopilotStatusLabel("future_status")).toBe("future_status");
    expect(autopilotExecutionModeLabel("")).toBe("Unknown");
    expect(formatTriggerKinds(undefined)).toBe("No triggers");
    expect(formatTriggerKinds([])).toBe("No triggers");
    expect(formatTriggerKinds(["schedule", "webhook"])).toBe(
      "Schedule, Webhook",
    );
  });
});
