export const PLATFORM_NAV_ICON_MAP = {
  inbox: {
    ios: { unfocused: "tray", focused: "tray.fill" },
    android: { unfocused: "file-tray-outline", focused: "file-tray" },
  },
  myIssues: {
    ios: { unfocused: "checklist.unchecked", focused: "checklist" },
    android: { unfocused: "checkbox-outline", focused: "checkbox" },
  },
  chat: {
    ios: { unfocused: "bubble.left", focused: "bubble.left.fill" },
    android: { unfocused: "chatbubble-outline", focused: "chatbubble" },
  },
  more: {
    ios: { unfocused: "ellipsis", focused: "ellipsis" },
    android: { unfocused: "ellipsis-horizontal", focused: "ellipsis-horizontal" },
  },
  pinned: {
    ios: { unfocused: "pin", focused: "pin.fill" },
    android: { unfocused: "pin-outline", focused: "pin" },
  },
  issues: {
    ios: { unfocused: "list.bullet", focused: "list.bullet" },
    android: { unfocused: "list-outline", focused: "list" },
  },
  projects: {
    ios: { unfocused: "square.stack", focused: "square.stack.fill" },
    android: { unfocused: "folder-outline", focused: "folder" },
  },
  autopilots: {
    ios: { unfocused: "clock", focused: "clock.fill" },
    android: { unfocused: "time-outline", focused: "time" },
  },
  chevron: {
    ios: { unfocused: "chevron.right", focused: "chevron.right" },
    android: { unfocused: "chevron-forward", focused: "chevron-forward" },
  },
  checkmark: {
    ios: { unfocused: "checkmark", focused: "checkmark" },
    android: { unfocused: "checkmark", focused: "checkmark" },
  },
} as const;

export type PlatformNavIconName = keyof typeof PLATFORM_NAV_ICON_MAP;
export type PlatformNavIconPlatform = "ios" | "android";

const FALLBACK = {
  ios: { unfocused: "questionmark.circle", focused: "questionmark.circle.fill" },
  android: { unfocused: "help-circle-outline", focused: "help-circle-outline" },
} as const;

export function getPlatformNavIconDefinition(
  name: string,
  platform: PlatformNavIconPlatform,
  focused = false,
): string {
  const mapping = PLATFORM_NAV_ICON_MAP[name as PlatformNavIconName] ?? FALLBACK;
  return mapping[platform][focused ? "focused" : "unfocused"];
}
