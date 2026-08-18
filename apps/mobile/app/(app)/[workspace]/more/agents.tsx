/**
 * Agents screen — reached from the More popover. Lists every agent in the
 * workspace (same query the chat agent picker uses, `agentListOptions`),
 * with presence dot, name, description, and a runtime-bound hint for
 * agents that can't run yet.
 *
 * Tap: starts a new chat with that agent — hands the selection to the
 * chat tab via `useChatAgentSelectStore` (one-shot request, same pattern
 * as the chat-sessions picker sheet), then switches to the Chat tab.
 *
 * Archived agents render at the bottom in reduced emphasis; tapping them
 * is a no-op (mirrors the agent-picker sheet's disabled state).
 */
import { useMemo } from "react";
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  View,
} from "react-native";
import { router } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import type { Agent } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { ActorAvatar } from "@/components/ui/actor-avatar";
import { agentListOptions } from "@/data/queries/agents";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useChatAgentSelectStore } from "@/data/stores/chat-agent-select-store";
import { isAgentRuntimeBound } from "@/lib/is-agent-runtime-bound";
import { cn } from "@/lib/utils";

export default function AgentsPage() {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);

  const { data, isLoading, error, refetch } = useQuery(
    agentListOptions(wsId),
  );

  const { live, archived } = useMemo(() => {
    const list = data ?? [];
    return {
      live: list.filter((a) => !a.archived_at),
      archived: list.filter((a) => !!a.archived_at),
    };
  }, [data]);

  const onPickAgent = (agent: Agent) => {
    if (!wsSlug) return;
    useChatAgentSelectStore.getState().requestSelect(agent.id);
    router.replace(`/${wsSlug}/chat`);
  };

  if (isLoading && !data) {
    return (
      <View className="flex-1 items-center justify-center bg-background">
        <ActivityIndicator />
      </View>
    );
  }

  if (error) {
    return (
      <View className="flex-1 bg-background px-4 gap-3 pt-4">
        <Text className="text-sm text-destructive">
          Failed to load agents:{" "}
          {error instanceof Error ? error.message : "unknown error"}
        </Text>
        <Button variant="outline" onPress={() => refetch()}>
          <Text>Retry</Text>
        </Button>
      </View>
    );
  }

  if (live.length === 0 && archived.length === 0) {
    return (
      <View className="flex-1 items-center justify-center bg-background px-6">
        <Text className="text-sm text-muted-foreground text-center">
          No agents in this workspace yet.
        </Text>
      </View>
    );
  }

  return (
    <ScrollView
      className="flex-1 bg-background"
      contentContainerClassName="pb-6"
      showsVerticalScrollIndicator={false}
    >
      {live.map((agent, idx) => (
        <View key={agent.id}>
          {idx > 0 ? <View className="h-px bg-border ml-4" /> : null}
          <AgentRow agent={agent} onPress={() => onPickAgent(agent)} />
        </View>
      ))}
      {archived.length > 0 ? (
        <>
          <View className="px-4 pt-4 pb-1">
            <Text className="text-xs uppercase tracking-wider text-muted-foreground font-medium">
              Archived
            </Text>
          </View>
          {archived.map((agent, idx) => (
            <View key={agent.id}>
              {idx > 0 ? <View className="h-px bg-border ml-4" /> : null}
              <AgentRow agent={agent} onPress={() => {}} />
            </View>
          ))}
        </>
      ) : null}
    </ScrollView>
  );
}

function AgentRow({
  agent,
  onPress,
}: {
  agent: Agent;
  onPress: () => void;
}) {
  const runtimeBound = isAgentRuntimeBound(agent);
  return (
    <Pressable
      onPress={onPress}
      disabled={!runtimeBound || !!agent.archived_at}
      className={cn(
        "flex-row items-center gap-3 px-4 py-3 active:bg-secondary",
        (!runtimeBound || !!agent.archived_at) && "opacity-50",
      )}
      accessibilityRole="button"
      accessibilityLabel={`Chat with ${agent.name}`}
      accessibilityState={{ disabled: !runtimeBound || !!agent.archived_at }}
    >
      <ActorAvatar type="agent" id={agent.id} size={36} showPresence />
      <View className="flex-1 min-w-0">
        <Text className="text-sm font-medium text-foreground" numberOfLines={1}>
          {agent.name}
        </Text>
        {agent.description ? (
          <Text
            className="text-xs text-muted-foreground"
            numberOfLines={1}
          >
            {agent.description}
          </Text>
        ) : null}
        {!runtimeBound ? (
          <Text className="text-xs text-muted-foreground">
            Needs a runtime to chat
          </Text>
        ) : null}
      </View>
    </Pressable>
  );
}