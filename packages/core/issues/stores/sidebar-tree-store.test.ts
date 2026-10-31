import { beforeEach, describe, expect, it } from "vitest";
import { DEFAULT_SIDEBAR_ISSUE_SETTINGS, useSidebarTreeStore } from "./sidebar-tree-store";

beforeEach(() => {
  useSidebarTreeStore.setState({ expanded: [], issueSettings: DEFAULT_SIDEBAR_ISSUE_SETTINGS });
});

describe("useSidebarTreeStore", () => {
  it("patches the shared issue settings", () => {
    const store = useSidebarTreeStore.getState();
    store.setIssueSettings({ sortBy: "priority" });
    store.setIssueSettings({ assignee: "me" });
    expect(useSidebarTreeStore.getState().issueSettings).toEqual({
      ...DEFAULT_SIDEBAR_ISSUE_SETTINGS,
      sortBy: "priority",
      assignee: "me",
    });
  });

  it("toggles expansion idempotently", () => {
    const store = useSidebarTreeStore.getState();
    store.setExpanded("project:p1", true);
    store.setExpanded("project:p1", true);
    expect(useSidebarTreeStore.getState().expanded).toEqual(["project:p1"]);
    store.setExpanded("project:p1", false);
    expect(useSidebarTreeStore.getState().expanded).toEqual([]);
  });

  it("collapses each section on its own", () => {
    useSidebarTreeStore.setState({ viewsCollapsed: false, pinnedCollapsed: false, projectsCollapsed: false });
    useSidebarTreeStore.getState().setPinnedCollapsed(true);
    const state = useSidebarTreeStore.getState();
    expect([state.viewsCollapsed, state.pinnedCollapsed, state.projectsCollapsed]).toEqual([false, true, false]);
    state.setViewsCollapsed(true);
    expect(useSidebarTreeStore.getState().viewsCollapsed).toBe(true);
  });
});
