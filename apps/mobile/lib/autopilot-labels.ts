/**
 * Display labels for autopilot enums. Unknown server values fall through to
 * a generic string — never exhaustively switch (API Response Compatibility).
 */

export function autopilotStatusLabel(status: string): string {
  switch (status) {
    case "active":
      return "Active";
    case "paused":
      return "Paused";
    case "archived":
      return "Archived";
    default:
      return status || "Unknown";
  }
}

export function autopilotExecutionModeLabel(mode: string): string {
  switch (mode) {
    case "create_issue":
      return "Create issue";
    case "run_only":
      return "Run only";
    default:
      return mode || "Unknown";
  }
}

export function autopilotRunStatusLabel(status: string): string {
  switch (status) {
    case "issue_created":
      return "Issue created";
    case "running":
      return "Running";
    case "completed":
      return "Completed";
    case "failed":
      return "Failed";
    case "skipped":
      return "Skipped";
    default:
      return status || "Unknown";
  }
}

export function autopilotTriggerKindLabel(kind: string): string {
  switch (kind) {
    case "schedule":
      return "Schedule";
    case "webhook":
      return "Webhook";
    case "api":
      return "API";
    case "manual":
      return "Manual";
    default:
      return kind || "Unknown";
  }
}

export function formatTriggerKinds(kinds: string[] | undefined): string {
  if (!kinds || kinds.length === 0) return "No triggers";
  return kinds.map(autopilotTriggerKindLabel).join(", ");
}
