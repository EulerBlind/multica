/**
 * Autopilot detail screen (read-only). Shows title, status, mode,
 * description, triggers, and recent runs. Create/edit/trigger stay on web.
 */
import { useCallback } from "react";
import {
  ActivityIndicator,
  RefreshControl,
  ScrollView,
  View,
} from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";
import { Stack, router, useLocalSearchParams } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import {
  autopilotDetailOptions,
  autopilotRunsOptions,
} from "@/data/queries/autopilots";
import { useWorkspaceStore } from "@/data/workspace-store";
import {
  autopilotExecutionModeLabel,
  autopilotRunStatusLabel,
  autopilotStatusLabel,
  autopilotTriggerKindLabel,
} from "@/lib/autopilot-labels";
import { timeAgo } from "@/lib/time-ago";

export default function AutopilotDetailPage() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);

  const detail = useQuery(autopilotDetailOptions(wsId, id));
  const runs = useQuery(autopilotRunsOptions(wsId, id));

  const onRefresh = useCallback(async () => {
    await Promise.all([detail.refetch(), runs.refetch()]);
  }, [detail, runs]);

  const autopilot = detail.data?.autopilot;
  const triggers = detail.data?.triggers ?? [];
  const missing = !autopilot || autopilot.id === "";

  return (
    <SafeAreaView className="flex-1 bg-background" edges={["bottom"]}>
      <Stack.Screen
        options={{
          title: autopilot?.title || "Autopilot",
          headerBackTitle: "Back",
        }}
      />

      {detail.isLoading ? (
        <View className="flex-1 items-center justify-center">
          <ActivityIndicator />
        </View>
      ) : detail.error || missing ? (
        <View className="flex-1 items-center justify-center px-6 gap-3">
          <Text className="text-sm text-destructive text-center">
            Failed to load autopilot:{" "}
            {detail.error instanceof Error
              ? detail.error.message
              : "not found"}
          </Text>
          <Button variant="outline" onPress={() => detail.refetch()}>
            <Text>Retry</Text>
          </Button>
          <Button variant="ghost" onPress={() => router.back()}>
            <Text>Go back</Text>
          </Button>
        </View>
      ) : (
        <ScrollView
          className="flex-1"
          contentContainerClassName="pb-8"
          refreshControl={
            <RefreshControl
              refreshing={detail.isRefetching || runs.isRefetching}
              onRefresh={onRefresh}
            />
          }
        >
          <View className="px-4 pt-4 gap-2">
            <Text className="text-xl font-semibold text-foreground">
              {autopilot.title}
            </Text>
            <View className="flex-row flex-wrap gap-x-3 gap-y-1">
              <MetaChip label={autopilotStatusLabel(autopilot.status)} />
              <MetaChip
                label={autopilotExecutionModeLabel(autopilot.execution_mode)}
              />
            </View>
            {autopilot.description ? (
              <Text className="text-sm text-muted-foreground mt-2">
                {autopilot.description}
              </Text>
            ) : null}
            <View className="mt-3 gap-1">
              {autopilot.next_run_at ? (
                <Text className="text-xs text-muted-foreground">
                  Next run: {timeAgo(autopilot.next_run_at)}
                </Text>
              ) : null}
              {autopilot.last_run_at ? (
                <Text className="text-xs text-muted-foreground">
                  Last run: {timeAgo(autopilot.last_run_at)}
                  {autopilot.last_run_status
                    ? ` · ${autopilotRunStatusLabel(autopilot.last_run_status)}`
                    : ""}
                </Text>
              ) : (
                <Text className="text-xs text-muted-foreground">
                  Never run
                </Text>
              )}
            </View>
          </View>

          <Section title="Triggers">
            {triggers.length === 0 ? (
              <Text className="text-sm text-muted-foreground px-4">
                No triggers configured.
              </Text>
            ) : (
              triggers.map((trigger) => (
                <View
                  key={trigger.id}
                  className="px-4 py-3 border-b border-border gap-1"
                >
                  <View className="flex-row items-center justify-between gap-2">
                    <Text className="text-sm font-medium text-foreground">
                      {autopilotTriggerKindLabel(trigger.kind)}
                      {trigger.label ? ` · ${trigger.label}` : ""}
                    </Text>
                    <Text className="text-xs text-muted-foreground">
                      {trigger.enabled ? "Enabled" : "Disabled"}
                    </Text>
                  </View>
                  {trigger.kind === "schedule" && trigger.cron_expression ? (
                    <Text className="text-xs text-muted-foreground">
                      {trigger.cron_expression}
                      {trigger.timezone ? ` (${trigger.timezone})` : ""}
                    </Text>
                  ) : null}
                  {trigger.next_run_at ? (
                    <Text className="text-xs text-muted-foreground">
                      Next: {timeAgo(trigger.next_run_at)}
                    </Text>
                  ) : null}
                </View>
              ))
            )}
          </Section>

          <Section title="Recent runs">
            {runs.isLoading ? (
              <View className="py-4 items-center">
                <ActivityIndicator />
              </View>
            ) : runs.error ? (
              <View className="px-4 gap-2">
                <Text className="text-sm text-destructive">
                  Failed to load runs:{" "}
                  {runs.error instanceof Error
                    ? runs.error.message
                    : "unknown error"}
                </Text>
                <Button variant="outline" onPress={() => runs.refetch()}>
                  <Text>Retry</Text>
                </Button>
              </View>
            ) : (runs.data?.length ?? 0) === 0 ? (
              <Text className="text-sm text-muted-foreground px-4">
                No runs yet.
              </Text>
            ) : (
              runs.data!.map((run) => (
                <View
                  key={run.id}
                  className="px-4 py-3 border-b border-border gap-1"
                >
                  <View className="flex-row items-center justify-between gap-2">
                    <Text className="text-sm font-medium text-foreground">
                      {autopilotRunStatusLabel(run.status)}
                    </Text>
                    <Text className="text-xs text-muted-foreground">
                      {run.triggered_at ? timeAgo(run.triggered_at) : "—"}
                    </Text>
                  </View>
                  <Text className="text-xs text-muted-foreground">
                    Source: {autopilotTriggerKindLabel(run.source)}
                  </Text>
                  {run.failure_reason ? (
                    <Text className="text-xs text-destructive" numberOfLines={3}>
                      {run.failure_reason}
                    </Text>
                  ) : null}
                </View>
              ))
            )}
          </Section>
        </ScrollView>
      )}
    </SafeAreaView>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <View className="mt-6">
      <Text className="px-4 pb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        {title}
      </Text>
      {children}
    </View>
  );
}

function MetaChip({ label }: { label: string }) {
  return (
    <View className="rounded-md bg-secondary px-2 py-0.5">
      <Text className="text-xs text-foreground">{label}</Text>
    </View>
  );
}
