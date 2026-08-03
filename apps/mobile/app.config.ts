import type { ExpoConfig, ConfigContext } from "expo/config";

/**
 * Dynamic Expo config — replaces app.json so we can read APP_ENV at runtime
 * and switch identifiers / display name for dev / staging / production /
 * private builds.
 *
 * APP_ENV is set by package.json scripts:
 *   - dev          → APP_ENV unset (treated as "development")
 *   - dev:staging  → APP_ENV=staging
 *   - dev:prod     → APP_ENV=production (rare; usually only for EAS build)
 */
export default ({ config }: ConfigContext): ExpoConfig => {
  const env = process.env.APP_ENV ?? "development";
  const isProd = env === "production";
  const isStaging = env === "staging";
  const isPrivate = env === "private";

  const androidPackage = isPrivate
    ? (process.env.EXPO_ANDROID_PACKAGE_PRIVATE ?? "ai.multica.mobile.privateapp")
    : isProd
      ? (process.env.EXPO_ANDROID_PACKAGE_PROD ?? "ai.multica.mobile")
      : isStaging
        ? "ai.multica.mobile.staging"
        : (process.env.EXPO_ANDROID_PACKAGE_DEV ?? "ai.multica.mobile.dev");

  return {
    ...config,
    name: isPrivate
      ? "multica"
      : isProd
        ? "Multica"
        : isStaging
          ? "Multica (Staging)"
          : "Multica (Dev)",
    slug: "multica-mobile",
    version: "0.1.0",
    orientation: "portrait",
    userInterfaceStyle: "automatic",
    scheme: isPrivate ? "multica-private" : "multica",
    // 1024x1024 source shared with the desktop client
    // (apps/desktop/build/icon.png). Expo prebuild generates every required
    // iOS icon size from this single PNG.
    icon: "./assets/icon.png",
    ios: {
      // Expo keeps the top-level portrait policy for iPhone while adding all
      // iPad orientations required for multitasking when tablet support is on.
      supportsTablet: true,
      // Pins DEVELOPMENT_TEAM on every prebuild. Leaving it unset is the normal
      // path — `expo run:ios` then resolves a signing identity from the Keychain
      // itself, which is right when the Apple ID owns exactly one team. With
      // several (a personal team plus an employer's) it takes the *first*
      // identity found whenever the terminal is non-interactive, writes that
      // choice into the generated ios/, and never clears it again: prebuild only
      // writes DEVELOPMENT_TEAM when a value is present, so a project pinned to
      // the wrong team stays wrong until ios/ is deleted. Setting this re-applies
      // the intended team on every `scripts/ios-run.sh` run, which also repairs
      // an already-mispinned checkout.
      appleTeamId: process.env.EXPO_APPLE_TEAM_ID,
      // Per-variant bundle id overrides exist for one reason: an Apple ID
      // can only sign bundle prefixes it owns, so contributors not on the
      // Multica Apple Developer team (and external users self-building a
      // personal copy against production) need to swap to a reverse-domain
      // they control. Each variant has its own `_<VARIANT>` suffix and is
      // only read inside that variant's branch — a generic
      // `EXPO_BUNDLE_IDENTIFIER` would leak across variants (Expo CLI
      // auto-loads `.env.<mode>.local` regardless of APP_ENV) and collapse
      // dev / staging / prod onto a single id.
      bundleIdentifier: isPrivate
        ? (process.env.EXPO_BUNDLE_IDENTIFIER_PRIVATE ?? "ai.multica.mobile.privateapp")
        : isProd
          ? (process.env.EXPO_BUNDLE_IDENTIFIER_PROD ?? "ai.multica.mobile")
          : isStaging
            ? "ai.multica.mobile.staging"
            : (process.env.EXPO_BUNDLE_IDENTIFIER_DEV ?? "ai.multica.mobile.dev"),
    },
    android: {
      package: androidPackage,
      permissions: ["android.permission.CAMERA"],
      // Expo's generated base manifest includes this optional development
      // permission. The private app does not draw over other applications, so
      // keep a manifest-merger removal rule in every generated Android build.
      blockedPermissions: ["android.permission.SYSTEM_ALERT_WINDOW"],
      icon: "./assets/icon-android-legacy.png",
      adaptiveIcon: {
        foregroundImage: "./assets/icon-android-foreground.png",
        backgroundColor: "#111827",
        monochromeImage: "./assets/icon-android-monochrome.png",
      },
    },
    plugins: [
      "expo-router",
      "expo-secure-store",
      "@react-native-community/datetimepicker",
      "react-native-enriched-markdown",
      [
        "expo-image-picker",
        {
          // Picker permission strings. Camera is requested only when the
          // user taps Take Photo; microphone remains disabled because the
          // app captures still images only.
          // iOS NSPhotoLibraryUsageDescription. Without this string in
          // Info.plist, calling launchImageLibraryAsync hard-crashes on
          // iOS 14+.
          photosPermission:
            "Allow Multica to access your photos to attach images to issues and comments.",
          cameraPermission:
            "Allow Multica to use your camera to attach photos to issues and comments.",
          microphonePermission: false,
        },
      ],
      [
        "expo-build-properties",
        {
          ios: {
            buildReactNativeFromSource: true,
          },
          android: {
            compileSdkVersion: 36,
            targetSdkVersion: 36,
            buildToolsVersion: "36.0.0",
          },
        },
      ],
    ],
    extra: { APP_ENV: env },
  };
};
