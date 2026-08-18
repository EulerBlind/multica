/**
 * Pinned home tab — the mobile main screen.
 *
 * Replaces the former workspace-wide "Issues" tab (QIA-393) per the mobile
 * UI rework: the primary tab is now "Pinned", and the workspace's issues
 * remain visible on the main screen as a 5-item preview section.
 *
 * Layout (top → bottom):
 *   1. "Pinned" — the user's pinned issues/projects (up to 5), reusing the
 *      same row rendering as more/pins.tsx. "See all" pushes the full
 *      Pinned list (/more/pins).
 *   2. "Issues" — the 5 most recently updated workspace issues.
 *      "See all" pushes the full workspace Issues screen (/issues).
 *
 * Data sources are the same queries the pushed screens use
 * (`pinListOptions` / `issueListOptions`), so realtime updates keep this
 * preview fresh with no extra wiring.
 */
import { useMemo } from "react";
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  View,
} from "react-native";
import { useQuery } from "@tanstack/react-query";
import { router } from "expo-router";
import { Ionicons } from "@expo/vector-icons";
import type { Issue, PinnedItem, Project } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { Header } from "@/components/ui/header";
import { HeaderActions } from "@/components/ui/app-header-actions";
import { IssueRow } from "@/components/issue/issue-row";
import { ProjectRow } from "@/components/project/project-row";
import { pinListOptions } from "@/data/queries/pins";
import { issueListOptions, issueDetailOptions } from "@/data/queries/issues";
import { projectDetailOptions } from "@/data/queries/projects";
import { useAuthStore } from "@/data/auth-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useColorScheme } from "@/lib/use-color-scheme";
import { THEME } from "@/lib/theme";

/** Preview cap for both sections on the main screen. */
const HOME_SECTION_LIMIT = 5;

export default function PinnedHomePage() {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);
  const userId = useAuthStore((s) => s.user?.id ?? null);
  const { colorScheme } = useColorScheme();

  const pinsQuery = useQuery(pinListOptions(wsId, userId));
  const issuesQuery = useQuery(issueListOptions(wsId));

  const pins = useMemo(() => {
    const sorted = [...(pinsQuery.data ?? [])].sort(
      (a, b) => a.position - b.position,
    );
    return sorted.slice(0, HOME_SECTION_LIMIT);
  }, [pinsQuery.data]);

  // 5 most recently updated live issues (cancelled still shown — the full
  // screen groups by BOARD_STATUSES; the preview just wants the freshest).
  const previewIssues = useMemo(() => {
    const live = (issuesQuery.data ?? []).filter(
      (i) => i.status !== "cancelled",
    );
    return [...live]
      .sort(
        (a, b) =>
          new Date(b.updated_at ?? "").getTime() -
          new Date(a.updated_at ?? "").getTime(),
      )
      .slice(0, HOME_SECTION_LIMIT);
  }, [issuesQuery.data]);

  const loading =
    (pinsQuery.isLoading && pinsQuery.data === undefined) ||
    (issuesQuery.isLoading && issuesQuery.data === undefined);

  const error = pinsQuery.error ?? issuesQuery.error;

  return (
    <View className="flex-1 bg-background">
      <Header title="Pinned" right={<HeaderActions />} />
      {loading ? (
        <View className="flex-1 items-center justify-center">
          <ActivityIndicator />
        </View>
      ) : error ? (
        <View className="px-4 gap-3 pt-4">
          <Text className="text-sm text-destructive">
            Failed to load:{" "}
            {error instanceof Error ? error.message : "unknown error"}
          </Text>
          <Button
            variant="outline"
            onPress={() => {
              void pinsQuery.refetch();
              void issuesQuery.refetch();
            }}
          >
            <Text>Retry</Text>
          </Button>
        </View>
      ) : (
        <ScrollView
          className="flex-1"
          contentContainerClassName="pb-6"
          showsVerticalScrollIndicator={false}
        >
          <SectionHeader
            title="Pinned"
            onSeeAll={
              (pinsQuery.data?.length ?? 0) > 0
                ? () => wsSlug && router.push(`/${wsSlug}/more/pins`)
                : undefined
            }
          />
          {pins.length === 0 ? (
            <EmptySection
              icon="pin-outline"
              iconColor={THEME[colorScheme].mutedForeground}
              message="No pins yet. Pin an issue or project from its actions menu to surface it here."
            />
          ) : (
            pins.map((pin, idx) => (
              <View key={pin.id}>
                {idx > 0 ? <Divider /> : null}
                <PinRow pin={pin} wsId={wsId} wsSlug={wsSlug} />
              </View>
            ))
          )}

          <SectionHeader
            title="Issues"
            onSeeAll={() => wsSlug && router.push(`/${wsSlug}/issues`)}
          />
          {previewIssues.length === 0 ? (
            <EmptySection
              icon="list-outline"
              iconColor={THEME[colorScheme].mutedForeground}
              message="No issues in this workspace yet."
            />
          ) : (
            previewIssues.map((issue, idx) => (
              <View key={issue.id}>
                {idx > 0 ? <Divider /> : null}
                <IssueRow
                  issue={issue}
                  showStatus
                  onPress={() => {
                    if (wsSlug) router.push(`/${wsSlug}/issue/${issue.id}`);
                  }}
                />
              </View>
            ))
          )}
        </ScrollView>
      )}
    </View>
  );
}

function SectionHeader({
  title,
  onSeeAll,
}: {
  title: string;
  onSeeAll?: () => void;
}) {
  return (
    <View className="flex-row items-center justify-between px-4 pt-4 pb-1">
      <Text className="text-xs uppercase tracking-wider text-muted-foreground font-medium">
        {title}
      </Text>
      {onSeeAll ? (
        <Pressable
          onPress={onSeeAll}
          hitSlop={8}
          className="flex-row items-center gap-0.5 active:opacity-60"
          accessibilityRole="button"
        >
          <Text className="text-xs text-primary font-medium">See all</Text>
          <Ionicons name="chevron-forward" size={12} color="#3b82f6" />
        </Pressable>
      ) : null}
    </View>
  );
}

function EmptySection({
  icon,
  iconColor,
  message,
}: {
  icon: keyof typeof Ionicons.glyphMap;
  iconColor: string;
  message: string;
}) {
  return (
    <View className="items-center justify-center px-8 py-8 gap-2">
      <Ionicons name={icon} size={28} color={iconColor} />
      <Text className="text-sm text-muted-foreground text-center">
        {message}
      </Text>
    </View>
  );
}

function Divider() {
  return <View className="h-px bg-border ml-4" />;
}

/**
 * Renders a pinned row by item type — issue → IssueRow, project →
 * ProjectRow. Identical row resolution to more/pins.tsx (detail query per
 * row; a missing row renders a low-emphasis placeholder so dead pins stay
 * visible and removable).
 */
function PinRow({
  pin,
  wsId,
  wsSlug,
}: {
  pin: PinnedItem;
  wsId: string | null;
  wsSlug: string | null;
}) {
  if (pin.item_type === "issue") {
    return <IssuePinRow pin={pin} wsId={wsId} wsSlug={wsSlug} />;
  }
  return <ProjectPinRow pin={pin} wsId={wsId} wsSlug={wsSlug} />;
}

function IssuePinRow({
  pin,
  wsId,
  wsSlug,
}: {
  pin: PinnedItem;
  wsId: string | null;
  wsSlug: string | null;
}) {
  const { data, isLoading } = useQuery(issueDetailOptions(wsId, pin.item_id));
  const issue = data && data.id ? (data as Issue) : null;

  if (isLoading) return <SkeletonRow />;
  if (!issue) return <MissingPinRow />;

  return (
    <IssueRow
      issue={issue}
      showStatus
      onPress={() => {
        if (wsSlug) router.push(`/${wsSlug}/issue/${issue.id}`);
      }}
    />
  );
}

function ProjectPinRow({
  pin,
  wsId,
  wsSlug,
}: {
  pin: PinnedItem;
  wsId: string | null;
  wsSlug: string | null;
}) {
  const { data, isLoading } = useQuery(
    projectDetailOptions(wsId, pin.item_id),
  );
  const project = data && data.id ? (data as Project) : null;

  if (isLoading) return <SkeletonRow />;
  if (!project) return <MissingPinRow />;

  return (
    <ProjectRow
      project={project}
      onPress={() => {
        if (wsSlug) router.push(`/${wsSlug}/project/${project.id}`);
      }}
    />
  );
}

function SkeletonRow() {
  return (
    <View className="px-4 py-3 flex-row items-center gap-3">
      <View className="size-5 rounded bg-muted" />
      <View className="flex-1 h-4 rounded bg-muted" />
    </View>
  );
}

function MissingPinRow() {
  const { colorScheme } = useColorScheme();
  return (
    <View className="px-4 py-3 flex-row items-center gap-3 opacity-60">
      <Ionicons
        name="alert-circle-outline"
        size={18}
        color={THEME[colorScheme].mutedForeground}
      />
      <Text className="flex-1 text-sm text-muted-foreground" numberOfLines={1}>
        Unavailable item
      </Text>
    </View>
  );
}
