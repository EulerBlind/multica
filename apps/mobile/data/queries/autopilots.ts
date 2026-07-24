/**
 * Workspace autopilot queries. List + detail + recent runs — read-only
 * surface for the in-app Autopilots screens (More → Autopilots).
 *
 * Workspace-scoped via `wsId` so switching workspaces flips the cache
 * without a manual invalidate (root CLAUDE.md rule).
 */
import { queryOptions } from "@tanstack/react-query";
import { api } from "@/data/api";

export const autopilotKeys = {
  all: (wsId: string | null) => ["autopilots", wsId] as const,
  list: (wsId: string | null) => [...autopilotKeys.all(wsId), "list"] as const,
  detail: (wsId: string | null, id: string) =>
    [...autopilotKeys.all(wsId), "detail", id] as const,
  runs: (wsId: string | null, id: string) =>
    [...autopilotKeys.all(wsId), "runs", id] as const,
};

export const autopilotListOptions = (wsId: string | null) =>
  queryOptions({
    queryKey: autopilotKeys.list(wsId),
    queryFn: async ({ signal }) => {
      const res = await api.listAutopilots({ signal });
      return res.autopilots;
    },
    enabled: !!wsId,
  });

export const autopilotDetailOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: autopilotKeys.detail(wsId, id),
    queryFn: ({ signal }) => api.getAutopilot(id, { signal }),
    enabled: !!wsId && !!id,
  });

export const autopilotRunsOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: autopilotKeys.runs(wsId, id),
    queryFn: async ({ signal }) => {
      const res = await api.listAutopilotRuns(
        id,
        { limit: 20, offset: 0 },
        { signal },
      );
      return res.runs;
    },
    enabled: !!wsId && !!id,
  });
