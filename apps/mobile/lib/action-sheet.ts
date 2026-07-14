export type ActionItemRole = "default" | "destructive" | "cancel";

export interface ActionItem {
  key: string;
  label: string;
  role?: ActionItemRole;
  disabled?: boolean;
  onPress: () => void;
}

export interface ActionSheetRequest {
  title?: string;
  message?: string;
  items: ActionItem[];
}

export function getActionSheetIndexes(items: ActionItem[]): {
  cancelButtonIndex?: number;
  destructiveButtonIndex?: number | number[];
  disabledButtonIndices?: number[];
} {
  const cancelButtonIndex = items.findIndex((item) => item.role === "cancel");
  const destructive = items
    .map((item, index) => (item.role === "destructive" ? index : -1))
    .filter((index) => index >= 0);
  const disabledButtonIndices = items
    .map((item, index) => (item.disabled ? index : -1))
    .filter((index) => index >= 0);

  return {
    ...(cancelButtonIndex >= 0 ? { cancelButtonIndex } : {}),
    ...(destructive.length === 1
      ? { destructiveButtonIndex: destructive[0] }
      : destructive.length > 1
        ? { destructiveButtonIndex: destructive }
        : {}),
    ...(disabledButtonIndices.length > 0 ? { disabledButtonIndices } : {}),
  };
}

export function runActionItemOnce(
  items: ActionItem[],
  index: number,
  alreadyHandled: boolean,
): boolean {
  if (alreadyHandled) return true;
  const item = items[index];
  if (!item || item.disabled) return true;
  item.onPress();
  return true;
}
