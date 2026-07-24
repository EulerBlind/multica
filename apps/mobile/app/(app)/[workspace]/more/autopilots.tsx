/**
 * Autopilots browse page. Flat FlatList over workspace autopilots.
 *
 * In-app navigation target for More → Autopilots (replaces the previous
 * Linking.openURL deep-link into the web app). Read-only: create / edit /
 * schedule management stay on web; this screen is the reachable list +
 * drill-down into detail.
 *
 * Sort: active/paused first (archived last), then `updated_at` desc.
 */
import { useMemo } from "react";
import {
  ActivityIndicator,
  FlatList,
  RefreshControl,
  View,
} from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";
import { useQuery } from "@tanstack/react-query";
import { Stack, router } from "expo-router";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { AutopilotRow } from "@/components/autopilot/autopilot-row";
import { autopilotListOptions } from "@/data/queries/autopilots";
import { useWorkspaceStore } from "@/data/workspace-store";

export default function AutopilotsPage() {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);

  const { data, isLoading, error, refetch, isRefetching } = useQuery(
    autopilotListOptions(wsId),
  );

  const sorted = useMemo(() => {
    if (!data) return [];
    return [...data]
      .filter((a) => a.status !== "archived")
      .sort((a, b) => {
        if (a.status !== b.status) {
          // active before paused
          if (a.status === "active") return -1;
          if (b.status === "active") return 1;
        }
        return (
          new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime()
        );
      });
  }, [data]);

  return (
    <SafeAreaView className="flex-1 bg-background" edges={[]}>
      <Stack.Screen options={{ title: "Autopilots" }} />

      {isLoading ? (
        <View className="flex-1 items-center justify-center">
          <ActivityIndicator />
        </View>
      ) : error ? (
        <View className="px-4 gap-3 pt-4">
          <Text className="text-sm text-destructive">
            Failed to load autopilots:{" "}
            {error instanceof Error ? error.message : "unknown error"}
          </Text>
          <Button variant="outline" onPress={() => refetch()}>
            <Text>Retry</Text>
          </Button>
        </View>
      ) : sorted.length === 0 ? (
        <EmptyState />
      ) : (
        <FlatList
          data={sorted}
          keyExtractor={(item) => item.id}
          ItemSeparatorComponent={() => (
            <View className="h-px bg-border ml-4" />
          )}
          renderItem={({ item }) => (
            <AutopilotRow
              autopilot={item}
              onPress={() => {
                if (wsSlug) router.push(`/${wsSlug}/autopilot/${item.id}`);
              }}
            />
          )}
          refreshControl={
            <RefreshControl refreshing={isRefetching} onRefresh={refetch} />
          }
          contentContainerClassName="pb-6"
        />
      )}
    </SafeAreaView>
  );
}

function EmptyState() {
  return (
    <View className="flex-1 items-center justify-center px-6 gap-4">
      <Text className="text-base font-medium text-foreground">
        No autopilots yet
      </Text>
      <Text className="text-sm text-muted-foreground text-center">
        Scheduled and webhook automations in this workspace will show up here.
        Create and edit them on the web for now.
      </Text>
    </View>
  );
}
