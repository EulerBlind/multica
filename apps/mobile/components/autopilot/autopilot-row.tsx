/**
 * Autopilot list row. Mirrors ProjectRow: left icon + title / meta, right
 * status + relative time.
 */
import { Pressable, View } from "react-native";
import type { Autopilot } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { PlatformNavIcon } from "@/components/nav/platform-nav-icon";
import { useColorScheme } from "@/lib/use-color-scheme";
import { THEME } from "@/lib/theme";
import { timeAgo } from "@/lib/time-ago";
import {
  autopilotExecutionModeLabel,
  autopilotStatusLabel,
  formatTriggerKinds,
} from "@/lib/autopilot-labels";

interface Props {
  autopilot: Autopilot;
  onPress: () => void;
}

export function AutopilotRow({ autopilot, onPress }: Props) {
  const { colorScheme } = useColorScheme();
  const t = THEME[colorScheme];
  const triggers = formatTriggerKinds(autopilot.trigger_kinds);
  const mode = autopilotExecutionModeLabel(autopilot.execution_mode);

  return (
    <Pressable onPress={onPress} className="active:bg-secondary px-4 py-3">
      <View className="flex-row items-start gap-3">
        <View className="size-9 items-center justify-center rounded-lg bg-secondary">
          <PlatformNavIcon name="autopilots" color={t.foreground} size={18} />
        </View>
        <View className="flex-1 gap-1">
          <Text
            className="text-base text-foreground font-medium"
            numberOfLines={1}
          >
            {autopilot.title}
          </Text>
          <View className="flex-row items-center gap-3">
            <Text className="text-xs text-muted-foreground" numberOfLines={1}>
              {triggers}
            </Text>
            <Text className="text-xs text-muted-foreground" numberOfLines={1}>
              {mode}
            </Text>
          </View>
        </View>
        <View className="items-end gap-1">
          <Text className="text-xs text-muted-foreground">
            {autopilotStatusLabel(autopilot.status)}
          </Text>
          <Text className="text-[11px] text-muted-foreground/70">
            {autopilot.last_run_at
              ? timeAgo(autopilot.last_run_at)
              : autopilot.next_run_at
                ? `next ${timeAgo(autopilot.next_run_at)}`
                : "—"}
          </Text>
        </View>
      </View>
    </Pressable>
  );
}
