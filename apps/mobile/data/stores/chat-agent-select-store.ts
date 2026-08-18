/**
 * Cross-screen channel between the More → Agents list and the Chat tab.
 *
 * The chat tab keeps `selectedAgentId` in local state; the Agents screen
 * (a pushed route) needs a way to hand "user picked this agent" back down
 * to the tab tree. Same one-shot pattern as `chat-session-picker-store`:
 * the writer bumps `nonce` with the picked agent id; the chat tab
 * `useEffect`s on it, applies the selection, then `consume()`s.
 *
 * Workspace lifecycle: this store holds workspace-scoped state
 * (`selectAgentRequest` for the current workspace's chat tab). Reset is
 * wired in `app/(app)/[workspace]/_layout.tsx` via
 * `useResetOnWorkspaceChange()` — see chat-session-picker-store.
 */
import { useEffect, useRef } from "react";
import { create } from "zustand";

interface ChatAgentSelectState {
  /** One-shot request: agent id + nonce so re-picking the SAME agent after
   *  a consume still re-fires the chat tab's effect. */
  selectAgentRequest: { agentId: string; nonce: number } | null;
  requestSelect: (agentId: string) => void;
  consumeSelect: () => void;
  reset: () => void;
}

export const useChatAgentSelectStore = create<ChatAgentSelectState>(
  (set, get) => ({
    selectAgentRequest: null,
    requestSelect: (agentId) =>
      set({
        selectAgentRequest: {
          agentId,
          nonce: (get().selectAgentRequest?.nonce ?? 0) + 1,
        },
      }),
    consumeSelect: () => set({ selectAgentRequest: null }),
    reset: () => set({ selectAgentRequest: null }),
  }),
);

/**
 * Clears the pending agent-selection request whenever the active workspace
 * id changes. Mounted once from the workspace `_layout.tsx`; the `useRef`
 * gate makes the first mount a true no-op (see
 * `useChatSessionPickerResetOnWorkspaceChange` for the same pattern).
 */
export function useChatAgentSelectResetOnWorkspaceChange(wsId: string | null) {
  const prevRef = useRef(wsId);
  useEffect(() => {
    if (prevRef.current !== wsId) {
      useChatAgentSelectStore.getState().reset();
      prevRef.current = wsId;
    }
  }, [wsId]);
}