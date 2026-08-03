/**
 * Single row inside the agent-runs formSheet route
 * (`app/(app)/[workspace]/issue/[id]/runs.tsx`). Same component for active
 * and past tasks —
 * the trailing Cancel button is conditional on `status in {queued,
 * dispatched, running}`, and the status badge / colour swaps based on the
 * AgentTask.status enum.
 *
 * Tapping a row pushes the run-detail screen so the user can inspect the
 * execution process timeline (thinking / tool calls / errors) — the same
 * surface desktop exposes via AgentTranscriptDialog.
 */
import { Alert, Pressable, View } from "react-native";
import type { AgentTask } from "@multica/core/types";
import { router } from "expo-router";
import { Text } from "@/components/ui/text";
import { ActorAvatar } from "@/components/ui/actor-avatar";
import { useCancelTask, useRerunTask } from "@/data/mutations/issues";
import { useActorLookup } from "@/data/use-actor-name";
import { useWorkspaceStore } from "@/data/workspace-store";
import { runFailureBadgeLabel } from "@/lib/run-failure-badge";
import { timeAgo } from "@/lib/time-ago";

interface Props {
  task: AgentTask;
  issueId: string;
}

const ACTIVE_STATUSES: readonly AgentTask["status"][] = [
  "queued",
  "dispatched",
  "running",
];

// Failed and cancelled runs can be retried — mirrors web's execution-log
// retry (packages/views/issues/components/execution-log-section.tsx
// `canRetry = task.status === "failed" || task.status === "cancelled"`).
const RETRYABLE_STATUSES: readonly AgentTask["status"][] = [
  "failed",
  "cancelled",
];

export function RunRow({ task, issueId }: Props) {
  const { getName } = useActorLookup();
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);
  const isActive = ACTIVE_STATUSES.includes(task.status);
  const canRetry = RETRYABLE_STATUSES.includes(task.status);
  const summary = task.trigger_summary?.trim() || fallbackSummary(task);
  const timestamp = task.completed_at || task.created_at;

  const openDetail = () => {
    if (!wsSlug) return;
    router.push({
      pathname: "/[workspace]/issue/[id]/runs/[taskId]",
      params: { workspace: wsSlug, id: issueId, taskId: task.id },
    });
  };

  return (
    <View className="flex-row items-start gap-3 py-2">
      <Pressable
        onPress={openDetail}
        accessibilityRole="button"
        accessibilityLabel={`Open run details for ${getName("agent", task.agent_id)}`}
        className="flex-1 flex-row items-start gap-3 active:opacity-70"
      >
        <ActorAvatar type="agent" id={task.agent_id} size={28} showPresence />
        <View className="flex-1 gap-1">
          <Text className="text-sm text-foreground" numberOfLines={2}>
            <Text className="font-medium">{getName("agent", task.agent_id)}</Text>
            <Text className="text-muted-foreground"> · {summary}</Text>
          </Text>
          <View className="flex-row items-center gap-2">
            <StatusBadge task={task} />
            <Text className="text-xs text-muted-foreground">
              {timestamp ? timeAgo(timestamp) : ""}
            </Text>
          </View>
        </View>
      </Pressable>
      <View className="flex-row items-center gap-2">
        {canRetry ? <RetryButton taskId={task.id} issueId={issueId} /> : null}
        {isActive ? <CancelButton taskId={task.id} issueId={issueId} /> : null}
      </View>
    </View>
  );
}

function StatusBadge({ task }: { task: AgentTask }) {
  const label = STATUS_LABEL[task.status] ?? task.status;
  const cls = STATUS_CLASS[task.status] ?? "text-muted-foreground";
  // For failed tasks, surface the failure_reason inline so users don't have
  // to drill in. Missing / empty / unrecognised stays as just "Failed".
  if (task.status === "failed") {
    const reasonLabel = runFailureBadgeLabel(task.failure_reason);
    if (reasonLabel) {
      return (
        <Text className={`text-xs ${cls}`}>
          {label} · {reasonLabel}
        </Text>
      );
    }
  }
  return <Text className={`text-xs ${cls}`}>{label}</Text>;
}

function CancelButton({
  taskId,
  issueId,
}: {
  taskId: string;
  issueId: string;
}) {
  const mutation = useCancelTask(issueId);

  const onPress = () => {
    Alert.alert(
      "Cancel task?",
      "The agent will stop after the current step.",
      [
        { text: "Keep running", style: "cancel" },
        {
          text: "Cancel task",
          style: "destructive",
          onPress: () => mutation.mutate(taskId),
        },
      ],
    );
  };

  return (
    <Pressable
      onPress={onPress}
      disabled={mutation.isPending}
      className="px-3 py-1.5 rounded-md bg-secondary active:opacity-70"
    >
      <Text className="text-xs font-medium text-foreground">Cancel</Text>
    </Pressable>
  );
}

/**
 * Retry a failed/cancelled agent run — fires the same mutation web's
 * execution-log Retry button uses (`rerunIssue` → POST /api/issues/:id/rerun
 * with the source task id). No confirmation dialog: web triggers the rerun
 * directly on click, and the run-detail screen is one tap away if the user
 * wants to inspect the previous failure first.
 */
function RetryButton({
  taskId,
  issueId,
}: {
  taskId: string;
  issueId: string;
}) {
  const mutation = useRerunTask(issueId);

  const onPress = () => {
    if (mutation.isPending) return;
    mutation.mutate(taskId, {
      onError: (err) => {
        Alert.alert(
          "Retry failed",
          err instanceof Error ? err.message : "Could not rerun this task.",
        );
      },
    });
  };

  return (
    <Pressable
      onPress={onPress}
      disabled={mutation.isPending}
      accessibilityRole="button"
      accessibilityLabel="Retry task"
      className="px-3 py-1.5 rounded-md bg-secondary active:opacity-70"
    >
      <Text className="text-xs font-medium text-foreground">
        {mutation.isPending ? "Retrying…" : "Retry"}
      </Text>
    </Pressable>
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

const STATUS_CLASS: Record<AgentTask["status"], string> = {
  queued: "text-muted-foreground",
  dispatched: "text-brand",
  waiting_local_directory: "text-muted-foreground",
  running: "text-brand",
  completed: "text-muted-foreground",
  failed: "text-destructive",
  cancelled: "text-muted-foreground",
};
