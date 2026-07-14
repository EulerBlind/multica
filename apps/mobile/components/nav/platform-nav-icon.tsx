import type { ComponentProps } from "react";
import { Platform } from "react-native";
import { Image } from "expo-image";
import { Ionicons } from "@expo/vector-icons";

import {
  getPlatformNavIconDefinition,
  type PlatformNavIconName,
} from "./platform-nav-icon-map";

type IoniconName = ComponentProps<typeof Ionicons>["name"];

export function PlatformNavIcon({
  name,
  color,
  size,
  focused = false,
  accessibilityLabel,
  decorative = true,
}: {
  name: PlatformNavIconName;
  color: string;
  size: number;
  focused?: boolean;
  accessibilityLabel?: string;
  decorative?: boolean;
}) {
  const accessibility = decorative
    ? {
        accessible: false,
        accessibilityElementsHidden: true,
        importantForAccessibility: "no-hide-descendants" as const,
      }
    : { accessible: true, accessibilityLabel };

  if (Platform.OS === "ios") {
    const symbol = getPlatformNavIconDefinition(name, "ios", focused);
    return (
      <Image
        source={`sf:${symbol}`}
        tintColor={color}
        style={{ width: size, height: size }}
        {...accessibility}
      />
    );
  }

  const glyph = getPlatformNavIconDefinition(name, "android", focused) as IoniconName;
  return <Ionicons name={glyph} color={color} size={size} {...accessibility} />;
}
