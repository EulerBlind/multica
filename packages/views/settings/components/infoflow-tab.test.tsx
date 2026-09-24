// @vitest-environment jsdom

import { type ReactNode } from "react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

type MemberRole = "owner" | "admin" | "member";

const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as MemberRole }],
}));
const installationsRef = vi.hoisted(() => ({
  current: {
    installations: [] as unknown[],
    configured: true,
    install_supported: true,
  },
}));
const mockRegisterBYO = vi.hoisted(() => vi.fn());
const mockDeleteInstallation = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());
const mockToastSuccess = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[]; enabled?: boolean }) => {
    if (opts.enabled === false) return { data: undefined, isLoading: false };
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current, isLoading: false };
    if (key.includes("installations")) {
      return { data: installationsRef.current, isLoading: false };
    }
    return { data: undefined, isLoading: false };
  },
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getAgentName: (agentId: string) => `Agent ${agentId}`,
    getMemberName: () => "Unknown",
    getSquadName: () => "Unknown Squad",
    getActorName: () => "Unknown",
    getActorInitials: () => "??",
    getActorAvatarUrl: () => null,
  }),
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) =>
    sel({ user: { id: "user-1" } }),
}));

vi.mock("@multica/core/infoflow", () => ({
  infoflowInstallationsOptions: () => ({
    queryKey: ["infoflow", "installations"],
    queryFn: vi.fn(),
  }),
  infoflowKeys: {
    installations: (wsId: string) => ["infoflow", wsId, "installations"],
  },
}));

vi.mock("@multica/core/api", () => ({
  api: {
    registerInfoFlowBYO: mockRegisterBYO,
    deleteInfoFlowInstallation: mockDeleteInstallation,
  },
}));

vi.mock("sonner", () => ({
  toast: { success: mockToastSuccess, error: mockToastError },
}));

vi.mock("../../platform", () => ({ openExternal: vi.fn() }));

// The installation row renders ActorAvatar with `profileLink`, which resolves
// the workspace slug through @multica/core/paths. The real hook throws outside
// a workspace-scoped route, so the test supplies a slug like the runtime does.
vi.mock("@multica/core/paths", () => ({
  useRequiredWorkspaceSlug: () => "acme",
  useWorkspacePaths: () => ({
    agent: (id: string) => `/acme/agents/${id}`,
    agentDetail: (id: string) => `/acme/agents/${id}`,
  }),
}));

vi.mock("../../navigation/context", () => ({
  useNavigation: () => ({ push: vi.fn() }),
  useOptionalNavigation: () => ({ push: vi.fn() }),
}));

import { InfoFlowTab, InfoFlowAgentBindButton } from "./infoflow-tab";

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider
      locale="en"
      resources={{ en: { common: enCommon, settings: enSettings } }}
    >
      {children}
    </I18nProvider>
  );
}

describe("InfoFlowTab", () => {
  beforeEach(() => {
    membersRef.current = [{ user_id: "user-1", role: "owner" }];
    installationsRef.current = {
      installations: [],
      configured: true,
      install_supported: true,
    };
    mockRegisterBYO.mockReset();
    mockDeleteInstallation.mockReset();
    mockInvalidate.mockReset();
    mockToastSuccess.mockReset();
    mockToastError.mockReset();
  });

  afterEach(() => cleanup());

  it("renders the operator hint when the deployment has no secret key", () => {
    installationsRef.current = {
      installations: [],
      configured: false,
      install_supported: false,
    };
    render(<InfoFlowTab />, { wrapper: Wrapper });
    expect(screen.getByText(/InfoFlow integration not enabled/i)).toBeTruthy();
    expect(screen.getByText("MULTICA_INFOFLOW_SECRET_KEY")).toBeTruthy();
  });

  it("renders the empty state pointing at the agent page", () => {
    render(<InfoFlowTab />, { wrapper: Wrapper });
    expect(screen.getByText(/No bots connected yet/i)).toBeTruthy();
    expect(screen.getByText(/Connect InfoFlow/)).toBeTruthy();
  });

  it("lists an active installation with its application id", () => {
    installationsRef.current = {
      installations: [
        {
          id: "inst-1",
          workspace_id: "workspace-1",
          agent_id: "agent-1",
          application_id: "app-123",
          installer_user_id: "user-1",
          status: "active",
          installed_at: "2026-09-24T08:00:00Z",
          created_at: "2026-09-24T08:00:00Z",
          updated_at: "2026-09-24T08:00:00Z",
        },
      ],
      configured: true,
      install_supported: true,
    };
    render(<InfoFlowTab />, { wrapper: Wrapper });
    expect(screen.getByText("Agent agent-1")).toBeTruthy();
    expect(screen.getByText("app-123")).toBeTruthy();
  });

  it("submits a BYO install and invalidates the listing", async () => {
    const user = userEvent.setup();
    mockRegisterBYO.mockResolvedValue({ id: "inst-1" });
    render(<InfoFlowAgentBindButton agentId="agent-1" agentName="Release Bot" />, {
      wrapper: Wrapper,
    });
    await user.click(screen.getByTestId("infoflow-agent-connect"));
    await user.type(screen.getByTestId("infoflow-byo-application-id"), "app-123");
    await user.type(screen.getByTestId("infoflow-byo-app-key"), "key-1");
    await user.type(screen.getByTestId("infoflow-byo-app-secret"), "secret-1");
    await user.click(screen.getByTestId("infoflow-byo-submit"));
    await waitFor(() =>
      expect(mockRegisterBYO).toHaveBeenCalledWith("workspace-1", "agent-1", {
        application_id: "app-123",
        app_key: "key-1",
        app_secret: "secret-1",
        bot_user_id: undefined,
      }),
    );
    expect(mockInvalidate).toHaveBeenCalled();
    expect(mockToastSuccess).toHaveBeenCalled();
  });

  it("keeps the submit disabled until every required field is filled", async () => {
    const user = userEvent.setup();
    render(<InfoFlowAgentBindButton agentId="agent-1" agentName="Release Bot" />, {
      wrapper: Wrapper,
    });
    await user.click(screen.getByTestId("infoflow-agent-connect"));
    expect(screen.getByTestId("infoflow-byo-submit").hasAttribute("disabled")).toBe(true);
    await user.type(screen.getByTestId("infoflow-byo-application-id"), "app-123");
    await user.type(screen.getByTestId("infoflow-byo-app-key"), "key-1");
    expect(screen.getByTestId("infoflow-byo-submit").hasAttribute("disabled")).toBe(true);
    await user.type(screen.getByTestId("infoflow-byo-app-secret"), "secret-1");
    expect(screen.getByTestId("infoflow-byo-submit").hasAttribute("disabled")).toBe(false);
  });

  it("shows the connected badge with a disconnect affordance once installed", async () => {
    const user = userEvent.setup();
    installationsRef.current = {
      installations: [
        {
          id: "inst-1",
          workspace_id: "workspace-1",
          agent_id: "agent-1",
          application_id: "app-123",
          installer_user_id: "user-1",
          status: "active",
          installed_at: "2026-09-24T08:00:00Z",
          created_at: "2026-09-24T08:00:00Z",
          updated_at: "2026-09-24T08:00:00Z",
        },
      ],
      configured: true,
      install_supported: true,
    };
    mockDeleteInstallation.mockResolvedValue(undefined);
    render(<InfoFlowAgentBindButton agentId="agent-1" agentName="Release Bot" />, {
      wrapper: Wrapper,
    });
    expect(screen.getByTestId("infoflow-agent-bot-connected")).toBeTruthy();
    await user.click(screen.getByTestId("infoflow-agent-bot-disconnect"));
    await user.click(await screen.findByRole("button", { name: /^Disconnect$/ }));
    await waitFor(() =>
      expect(mockDeleteInstallation).toHaveBeenCalledWith("workspace-1", "inst-1"),
    );
  });

  it("hides the connect entry point for a member who cannot manage the agent", () => {
    membersRef.current = [{ user_id: "user-2", role: "member" }];
    render(
      <InfoFlowAgentBindButton
        agentId="agent-1"
        agentName="Release Bot"
        agentOwnerId="user-9"
      />,
      { wrapper: Wrapper },
    );
    expect(screen.queryByTestId("infoflow-agent-connect")).toBeNull();
  });
});
