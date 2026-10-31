import { fireEvent, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses/queries";
import {
  DEFAULT_SIDEBAR_ISSUE_SETTINGS,
  DEFAULT_SIDEBAR_PROJECT_SETTINGS,
  useSidebarTreeStore,
} from "@multica/core/issues/stores/sidebar-tree-store";
import { renderWithI18n } from "../../test/i18n";
import { ProjectsSectionSettings } from "./sidebar-settings";

vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () =>
    buildIssueStatusCatalog(
      [
        ["todo", "Todo", "unstarted"],
        ["in_progress", "In Progress", "started"],
        ["done", "Done", "done"],
      ].map(([key, name, category], position) => ({
        id: key!,
        workspace_id: "ws-1",
        key: key!,
        name: name!,
        description: "",
        category: category as "unstarted",
        color: "",
        is_system: true,
        position,
        archived_at: null,
        created_at: "",
        updated_at: "",
      })),
    ),
}));
vi.mock("../../issues/components/status-icon", () => ({ StatusIcon: () => <span /> }));

beforeEach(() => {
  useSidebarTreeStore.setState({
    issueSettings: DEFAULT_SIDEBAR_ISSUE_SETTINGS,
    projectSettings: DEFAULT_SIDEBAR_PROJECT_SETTINGS,
  });
});

const chip = (name: string) => screen.getByRole("button", { name });

describe("ProjectsSectionSettings", () => {
  it("covers both the project list and every project's issues", () => {
    renderWithI18n(<ProjectsSectionSettings wsId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: "Project display" }));

    expect(screen.getByRole("heading", { name: "Projects" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Issues in projects" })).toBeInTheDocument();
  });

  it("toggles issue statuses from their chips", () => {
    renderWithI18n(<ProjectsSectionSettings wsId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: "Project display" }));

    expect(chip("Todo")).toHaveAttribute("aria-pressed", "true");
    expect(chip("Done")).toHaveAttribute("aria-pressed", "false");

    fireEvent.click(chip("Todo"));
    const hidden = useSidebarTreeStore.getState().issueSettings.hiddenStatuses;
    expect(hidden).toContain("todo");
    expect(hidden).toContain("done");
    expect(chip("Todo")).toHaveAttribute("aria-pressed", "false");
  });

  it("toggles a project status and resets everything at once", () => {
    renderWithI18n(<ProjectsSectionSettings wsId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: "Project display" }));

    expect(screen.queryByRole("button", { name: "Reset" })).toBeNull();
    fireEvent.click(chip("Completed"));
    fireEvent.click(chip("Todo"));
    expect(useSidebarTreeStore.getState().projectSettings.statuses).toContain("completed");

    fireEvent.click(screen.getByRole("button", { name: "Reset" }));
    const state = useSidebarTreeStore.getState();
    expect(state.projectSettings).toEqual(DEFAULT_SIDEBAR_PROJECT_SETTINGS);
    expect(state.issueSettings).toEqual(DEFAULT_SIDEBAR_ISSUE_SETTINGS);
  });
});
