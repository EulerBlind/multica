import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import type { ReactNode } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../locales/en/common.json";

const TEST_RESOURCES = { en: { common: enCommon } };

const mockAuthState = vi.hoisted(() => ({
  user: null as { id: string; email: string } | null,
  isLoading: false,
}));

const mockNavigatePush = vi.hoisted(() => vi.fn());
const mockRedeemToken = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (sel?: (s: typeof mockAuthState) => unknown) =>
      sel ? sel(mockAuthState) : mockAuthState,
    { getState: () => mockAuthState },
  );
  return { useAuthStore };
});

vi.mock("../navigation/context", () => ({
  useNavigation: () => ({ push: mockNavigatePush }),
  useOptionalNavigation: () => ({ push: mockNavigatePush }),
}));

vi.mock("@multica/core/api", () => ({
  api: { redeemInfoFlowBindingToken: mockRedeemToken },
}));

import { InfoFlowBindPage } from "./bind-page";

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function renderPage(token: string | null) {
  return render(<InfoFlowBindPage token={token} />, { wrapper: I18nWrapper });
}

describe("InfoFlowBindPage", () => {
  beforeEach(() => {
    mockAuthState.user = null;
    mockAuthState.isLoading = false;
    mockNavigatePush.mockReset();
    mockRedeemToken.mockReset();
  });

  it("reports a missing token instead of calling the redeem endpoint", () => {
    renderPage(null);
    expect(screen.getByText(/missing its token/i)).toBeTruthy();
    expect(mockRedeemToken).not.toHaveBeenCalled();
  });

  it("asks an anonymous visitor to sign in before redeeming", () => {
    renderPage("tok-1");
    expect(screen.getByText(/Sign in to Multica/i)).toBeTruthy();
    expect(mockRedeemToken).not.toHaveBeenCalled();
  });

  it("redeems the token for a signed-in user and reports success", async () => {
    mockAuthState.user = { id: "user-1", email: "a@b.c" };
    mockRedeemToken.mockResolvedValue({
      workspace_id: "ws-1",
      installation_id: "inst-1",
      infoflow_user_id: "if-1",
    });
    renderPage("tok-1");
    await waitFor(() => expect(mockRedeemToken).toHaveBeenCalledWith("tok-1"));
    expect(await screen.findByText("You're linked.")).toBeTruthy();
  });

  it("maps an expired token error to the expired copy", async () => {
    mockAuthState.user = { id: "user-1", email: "a@b.c" };
    mockRedeemToken.mockRejectedValue(new Error("binding token invalid or expired"));
    renderPage("tok-1");
    expect(await screen.findByText(/invalid or expired/i)).toBeTruthy();
  });

  it("maps an already-bound error to the transfer copy", async () => {
    mockAuthState.user = { id: "user-1", email: "a@b.c" };
    mockRedeemToken.mockRejectedValue(
      new Error("this InfoFlow account is already bound to a different Multica user"),
    );
    renderPage("tok-1");
    expect(await screen.findByText(/already linked to a different/i)).toBeTruthy();
  });

  it("maps a non-member error to the workspace-membership copy", async () => {
    mockAuthState.user = { id: "user-1", email: "a@b.c" };
    mockRedeemToken.mockRejectedValue(new Error("binding refused (are you a workspace member?)"));
    renderPage("tok-1");
    expect(await screen.findByText(/isn't a member of this workspace/i)).toBeTruthy();
  });

  it("does not re-redeem the same token on a re-render", async () => {
    mockAuthState.user = { id: "user-1", email: "a@b.c" };
    mockRedeemToken.mockResolvedValue({
      workspace_id: "ws-1",
      installation_id: "inst-1",
      infoflow_user_id: "if-1",
    });
    const { rerender } = renderPage("tok-1");
    await waitFor(() => expect(mockRedeemToken).toHaveBeenCalledTimes(1));
    rerender(<InfoFlowBindPage token="tok-1" />);
    fireEvent.focus(window);
    expect(mockRedeemToken).toHaveBeenCalledTimes(1);
  });
});
