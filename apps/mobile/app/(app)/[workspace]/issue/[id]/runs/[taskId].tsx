/**
 * Agent run process-detail screen. Desktop opens
 * `AgentTranscriptDialog` (packages/views/common/task-transcript) from
 * the runs list — mobile previously left past-row taps as a no-op.
 *
 * Reuses the chat `ChatTimeline` + `taskMessagesOptions` stack so the
 * same thinking / tool_use / tool_result / error rows the chat tab
 * already renders are available for issue agent runs. Live tasks keep
 * growing via WS `task:message` when the issue realtime hook is active
 * on the parent; this screen also invalidates on focus.
 */
import { useCallback, useMemo } from "react";
import {
  ActivityIndicator,
  RefreshControl,
  ScrollView,
  View,
} from "react-native";
import { Stack, useLocalSearchParams } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import type { AgentTask } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { ActorAvatar } from "@/components/ui/actor-avatar";
import { ChatTimeline } from "@/components/chat/chat-timeline";
import {
  issueActiveTasksOptions,
  issueTasksOptions,
} from "@/data/queries/issues";
import { taskMessagesOptions } from "@/data/queries/chat";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useActorLookup } from "@/data/use-actor-name";
import { timeAgo } from "@/lib/time-ago";
import { appendTaskMessage } from "@/data/realtime/chat-ws-updaters";
import { useWSSubscriptions } from "@/lib/use-ws-subscriptions";
import { useQueryClient } from "@tanstack/react-query";
import type { TaskMessagePayload } from "@multica/core/types";

const ACTIVE_STATUSES: readonly AgentTask["status"][] = [
  "queued",
  "dispatched",
  "running",
  "waiting_local_directory",
];

export default function IssueRunDetailRoute() {
  const { id: issueId, taskId } = useLocalSearchParams<{
    id: string;
    taskId: string;
  }>();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { getName } = useActorLookup();
  const qc = useQueryClient();

  const { data: activeTasks = [] } = useQuery(
    issueActiveTasksOptions(wsId, issueId),
  );
  const { data: allTasks = [] } = useQuery(issueTasksOptions(wsId, issueId));

  const task = useMemo(() => {
    return (
      activeTasks.find((t) => t.id === taskId) ??
      allTasks.find((t) => t.id === taskId) ??
      null
    );
  }, [activeTasks, allTasks, taskId]);

  const messages = useQuery(taskMessagesOptions(taskId));
  const isLive = !!task && ACTIVE_STATUSES.includes(task.status);

  // Live append while this screen is open — mirrors chat's task:message
  // handler so a running issue agent keeps streaming process steps.
  useWSSubscriptions(
    (ws) => {
      if (!taskId) return;
      return [
        ws.on("task:message", (payload: unknown) => {
          const p = payload as TaskMessagePayload;
          if (p.task_id !== taskId) return;
          appendTaskMessage(qc, p);
        }),
      ];
    },
    [taskId, qc],
  );

  const onRefresh = useCallback(async () => {
    await messages.refetch();
  }, [messages]);

  const agentName = task ? getName("agent", task.agent_id) : "Agent";
  const summary = task?.trigger_summary?.trim() || (task ? fallbackSummary(task) : "");

  return (
    <View className="flex-1 bg-background">
      <Stack.Screen
        options={{
          title: "Run details",
          headerBackTitle: "Runs",
        }}
      />

      {!task ? (
        <View className="flex-1 items-center justify-center px-6 gap-3">
          <Text className="text-sm text-muted-foreground text-center">
            Run not found. It may have been cleaned up.
          </Text>
        </View>
      ) : (
        <ScrollView
          className="flex-1"
          contentContainerClassName="px-4 pb-8 gap-4"
          refreshControl={
            <RefreshControl
              refreshing={messages.isRefetching}
              onRefresh={onRefresh}
            />
          }
        >
          <View className="flex-row items-start gap-3 pt-4">
            <ActorAvatar type="agent" id={task.agent_id} size={36} showPresence />
            <View className="flex-1 gap-1">
              <Text className="text-base font-semibold text-foreground">
                {agentName}
              </Text>
              {summary ? (
                <Text className="text-sm text-muted-foreground" numberOfLines={3}>
                  {summary}
                </Text>
              ) : null}
              <View className="flex-row flex-wrap gap-x-3 gap-y-1 mt-1">
                <Text className="text-xs text-muted-foreground">
                  {STATUS_LABEL[task.status] ?? task.status}
                </Text>
                <Text className="text-xs text-muted-foreground">
                  {(task.completed_at || task.created_at)
                    ? timeAgo(task.completed_at || task.created_at)
                    : ""}
                </Text>
              </View>
              {task.status === "failed" && task.failure_reason ? (
                <Text className="text-xs text-destructive mt-1">
                  {task.failure_reason}
                </Text>
              ) : null}
            </View>
          </View>

          <View className="gap-2">
            <Text className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Execution process
            </Text>
            {messages.isLoading ? (
              <View className="py-8 items-center">
                <ActivityIndicator />
              </View>
            ) : messages.error ? (
              <View className="gap-2">
                <Text className="text-sm text-destructive">
                  Failed to load process details:{" "}
                  {messages.error instanceof Error
                    ? messages.error.message
                    : "unknown error"}
                </Text>
                <Button variant="outline" onPress={() => messages.refetch()}>
                  <Text>Retry</Text>
                </Button>
              </View>
            ) : (messages.data?.length ?? 0) === 0 ? (
              <Text className="text-sm text-muted-foreground">
                {isLive
                  ? "Waiting for the agent to start producing steps…"
                  : "No process steps were recorded for this run."}
              </Text>
            ) : (
              <ChatTimeline
                items={messages.data ?? []}
                isStreaming={isLive}
              />
            )}
          </View>
        </ScrollView>
      )}
    </View>
  );
}

function fallbackSummary(task: AgentTask): string {
  switch (task.kind) {
    case "comment":
      return "Comment task";
    case "autopilot":
      return "Autopilot run";
    case "chat":
      return "Chat task";
    case "quick_create":
      return "Quick create";
    case "direct":
    default:
      return "Task";
  }
}

const STATUS_LABEL: Record<AgentTask["status"], string> = {
  queued: "Queued",
  dispatched: "Starting",
  waiting_local_directory: "Waiting for directory",
  running: "Running",
  completed: "Done",
  failed: "Failed",
  cancelled: "Cancelled",
};
