export const REQUIRED_PRIVATE_ANDROID_PERMISSIONS = ["android.permission.CAMERA"];

export const FORBIDDEN_PRIVATE_ANDROID_PERMISSIONS = [
  "android.permission.RECORD_AUDIO",
  "android.permission.SYSTEM_ALERT_WINDOW",
];

export function parseAndroidPermissions(badging) {
  return [
    ...new Set(
      [...badging.matchAll(/^uses-permission(?:-sdk-\d+)?: name='([^']+)'/gm)].map(
        ([, permission]) => permission,
      ),
    ),
  ].sort();
}

export function assertPrivateAndroidPermissions(badging) {
  const permissions = parseAndroidPermissions(badging);

  for (const permission of REQUIRED_PRIVATE_ANDROID_PERMISSIONS) {
    if (!permissions.includes(permission)) {
      throw new Error(`${permission} permission is missing`);
    }
  }
  for (const permission of FORBIDDEN_PRIVATE_ANDROID_PERMISSIONS) {
    if (permissions.includes(permission)) {
      throw new Error(`${permission} must not be present`);
    }
  }

  return permissions;
}
