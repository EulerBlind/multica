import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export const REDACTED = "[REDACTED]";
export const MAX_SAFE_STREAM_CHARS = 64 * 1024;

const MAX_GIT_CAPTURE_BYTES = 8 * 1024 * 1024;
const SAFE_TRUNCATION_MARKER = "\n...[SAFE OUTPUT TRUNCATED]...\n";
const STRATEGIES = new Set(["ff-only", "merge", "rebase"]);
const SENSITIVE_QUERY_KEYS = new Set([
  "access_token",
  "api_key",
  "auth",
  "authorization",
  "auth_token",
  "client_secret",
  "credential",
  "key",
  "oauth_token",
  "password",
  "passwd",
  "private_token",
  "secret",
  "sig",
  "signature",
  "token",
  "x_api_key",
]);

const TOKEN_PATTERN =
  /(?<![A-Za-z0-9])(?:github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{10,}|xox[baprs]-[A-Za-z0-9-]{10,})/g;
const URL_USERINFO_PATTERN = /\b((?:https?|ssh|git):\/\/)[^/@\s'"]+@/gi;
const HEADER_PATTERN =
  /^(\s*(?:authorization|proxy-authorization|private-token|x-api-key|api-key|cookie|set-cookie)\s*:\s*)[^\r\n]+/gim;
const INLINE_KEY_VALUE_SECRET_PATTERN =
  /(?<boundary>^|[\s,;(\[{])(?<assignment>(?:[a-z0-9._-]+_(?:secret|token)|access_token|auth_token|oauth_token|client_secret|api_key|x-api-key|x_api_key|password|passwd|credential|signature|secret|token)\s*[:=]\s*)(?:(?<double>"(?:\\.|[^"\\\r\n])*")|(?<single>'(?:\\.|[^'\\\r\n])*')|(?<bare>[^\s,;&)\]}"']+)|(?<unclosedDouble>"[^"\r\n]*(?=\r?$))|(?<unclosedSingle>'[^'\r\n]*(?=\r?$)))/gim;
const QUERY_SECRET_PATTERN =
  /([?&](?:[a-z0-9._-]+_(?:secret|token)|access_token|auth_token|oauth_token|client_secret|api_key|x-api-key|x_api_key|auth|authorization|credential|key|password|passwd|signature|sig|secret|token)=)([^&#\s'"]+)/gi;

function strictPercentDecode(value) {
  if (/%(?![0-9A-Fa-f]{2})/.test(value)) {
    throw new Error("invalid percent encoding");
  }
  return decodeURIComponent(value);
}

function formDecode(value) {
  return strictPercentDecode(value.replaceAll("+", " "));
}

function formEncode(value) {
  return encodeURIComponent(value).replaceAll("%20", "+");
}

export function secretVariants(rawSecret, { encodedInput = false } = {}) {
  const decoded = new Set([rawSecret]);
  if (encodedInput) {
    decoded.add(strictPercentDecode(rawSecret));
    decoded.add(formDecode(rawSecret));
  }

  const variants = new Set();
  for (const value of decoded) {
    if (!value) continue;
    variants.add(value);
    variants.add(encodeURIComponent(value));
    variants.add(formEncode(value));
  }
  return variants;
}

export function isSensitiveQueryKey(rawKey) {
  const canonical = formDecode(rawKey).trim().toLowerCase().replaceAll("-", "_");
  return (
    SENSITIVE_QUERY_KEYS.has(canonical) ||
    canonical.endsWith("_secret") ||
    canonical.endsWith("_token")
  );
}

export function extractRemoteKnownSecrets(remoteUrl) {
  const secrets = new Set();
  if (!remoteUrl.includes("://")) {
    const scpLike = remoteUrl.match(/^([^@\s]+)@[^:\s]+:.+$/);
    if (scpLike) {
      for (const variant of secretVariants(scpLike[1])) secrets.add(variant);
    }
    return secrets;
  }

  let parsed;
  try {
    parsed = new URL(remoteUrl);
  } catch {
    throw new Error("remote URL has an invalid authority");
  }
  if (!parsed.protocol || !parsed.host || !parsed.hostname) {
    throw new Error("remote URL host is required");
  }
  if (parsed.port === "0") {
    throw new Error("remote URL port must be between 1 and 65535");
  }

  const authority = remoteUrl.slice(remoteUrl.indexOf("://") + 3).split(/[/?#]/, 1)[0];
  if (authority.includes("@")) {
    const rawUserinfo = authority.slice(0, authority.lastIndexOf("@"));
    for (const variant of secretVariants(rawUserinfo, { encodedInput: true })) secrets.add(variant);
    const separator = rawUserinfo.indexOf(":");
    const rawUsername = separator === -1 ? rawUserinfo : rawUserinfo.slice(0, separator);
    const rawPassword = separator === -1 ? undefined : rawUserinfo.slice(separator + 1);
    for (const variant of secretVariants(rawUsername, { encodedInput: true })) secrets.add(variant);
    if (rawPassword !== undefined) {
      for (const variant of secretVariants(rawPassword, { encodedInput: true })) secrets.add(variant);
    }
  }

  const queryStart = remoteUrl.indexOf("?");
  if (queryStart !== -1) {
    const rawQuery = remoteUrl.slice(queryStart + 1).split("#", 1)[0];
    for (const parameter of rawQuery.split("&")) {
      if (!parameter) continue;
      const separator = parameter.indexOf("=");
      if (separator === -1) continue;
      const rawKey = parameter.slice(0, separator);
      const rawValue = parameter.slice(separator + 1);
      if (!isSensitiveQueryKey(rawKey)) continue;
      for (const variant of secretVariants(rawValue, { encodedInput: true })) secrets.add(variant);
    }
  }

  return secrets;
}

function stableSecretSort(values) {
  return [...new Set(values)].filter(Boolean).sort((left, right) => {
    if (left.length !== right.length) return right.length - left.length;
    return left < right ? -1 : left > right ? 1 : 0;
  });
}

function redactInlineSecret(match, ...args) {
  const groups = args.at(-1);
  const prefix = `${groups.boundary}${groups.assignment}`;
  if (groups.double !== undefined || groups.unclosedDouble !== undefined) {
    return `${prefix}"${REDACTED}"`;
  }
  if (groups.single !== undefined || groups.unclosedSingle !== undefined) {
    return `${prefix}'${REDACTED}'`;
  }
  return `${prefix}${REDACTED}`;
}

export function sanitizeGitOutput(raw, knownSecrets = []) {
  let sanitized = String(raw ?? "");
  const variants = new Set();
  for (const secret of knownSecrets) {
    if (!secret) continue;
    for (const variant of secretVariants(secret)) variants.add(variant);
  }
  for (const secret of stableSecretSort(variants)) {
    sanitized = sanitized.split(secret).join(REDACTED);
  }

  sanitized = sanitized.replace(URL_USERINFO_PATTERN, `$1${REDACTED}@`);
  sanitized = sanitized.replace(HEADER_PATTERN, `$1${REDACTED}`);
  sanitized = sanitized.replace(INLINE_KEY_VALUE_SECRET_PATTERN, redactInlineSecret);
  sanitized = sanitized.replace(QUERY_SECRET_PATTERN, `$1${REDACTED}`);
  sanitized = sanitized.replace(TOKEN_PATTERN, REDACTED);
  return sanitized;
}

export function safeStream(raw, knownSecrets = []) {
  const sanitized = sanitizeGitOutput(raw, knownSecrets);
  if (sanitized.length <= MAX_SAFE_STREAM_CHARS) {
    return { text: sanitized, truncated: false };
  }
  const payloadSize = MAX_SAFE_STREAM_CHARS - SAFE_TRUNCATION_MARKER.length;
  const headSize = Math.floor(payloadSize / 2);
  const tailSize = payloadSize - headSize;
  return {
    text: `${sanitized.slice(0, headSize)}${SAFE_TRUNCATION_MARKER}${sanitized.slice(-tailSize)}`,
    truncated: true,
  };
}

function safeGitEnvironment(env) {
  return {
    ...env,
    GIT_ASKPASS: "",
    GIT_TERMINAL_PROMPT: "0",
    GCM_INTERACTIVE: "never",
    GIT_TRACE: "0",
    GIT_TRACE_PACKET: "0",
    GIT_TRACE_CURL: "0",
    GIT_CURL_VERBOSE: "0",
  };
}

function executeGit({ cwd, args, env, knownSecrets = [], spawn = spawnSync, captureOnly = false }) {
  const result = spawn("git", args, {
    cwd,
    env: safeGitEnvironment(env),
    encoding: "utf8",
    maxBuffer: MAX_GIT_CAPTURE_BYTES,
  });
  const exitCode = Number.isInteger(result.status) ? result.status : 1;
  if (captureOnly) {
    return {
      exitCode,
      stdout: String(result.stdout ?? ""),
      stderr: String(result.stderr ?? ""),
      failedToStart: Boolean(result.error),
    };
  }
  return {
    exitCode,
    stdout: safeStream(result.stdout, knownSecrets),
    stderr: safeStream(result.stderr, knownSecrets),
    failedToStart: Boolean(result.error),
  };
}

export class RemoteCredentialDiscoveryError extends Error {
  constructor(remote, exitCode, reason) {
    const diagnostic = {
      stage: "remote-credential-discovery",
      remote,
      exitCode,
      reason,
      recovery: "verify the named remote without printing its URL",
    };
    super(JSON.stringify(diagnostic));
    this.name = "RemoteCredentialDiscoveryError";
    this.diagnostic = diagnostic;
    this.exitCode = exitCode || 1;
  }
}

export class GitSyncError extends Error {
  constructor(diagnostic) {
    super(JSON.stringify(diagnostic));
    this.name = "GitSyncError";
    this.diagnostic = diagnostic;
    this.exitCode = diagnostic.exitCode || 1;
  }
}

export function discoverRemoteKnownSecrets({ cwd, remote, env = process.env, spawn = spawnSync }) {
  const result = executeGit({
    cwd,
    args: ["remote", "get-url", "--all", remote],
    env,
    spawn,
    captureOnly: true,
  });
  if (result.failedToStart || result.exitCode !== 0 || !result.stdout.trim()) {
    throw new RemoteCredentialDiscoveryError(
      remote,
      result.exitCode,
      "unable to resolve configured remote",
    );
  }

  const secrets = new Set();
  try {
    for (const remoteUrl of result.stdout.split(/\r?\n/)) {
      if (!remoteUrl) continue;
      for (const secret of extractRemoteKnownSecrets(remoteUrl)) secrets.add(secret);
    }
  } catch {
    throw new RemoteCredentialDiscoveryError(
      remote,
      result.exitCode,
      "unable to safely parse remote credentials",
    );
  }
  return stableSecretSort(secrets);
}

function commandFailureDiagnostic({ stage, result, recovery, context = {} }) {
  return {
    stage,
    ...context,
    exitCode: result.exitCode,
    reason: result.failedToStart ? "git is unavailable" : "git command failed",
    recovery,
    stdout: result.stdout.text,
    stderr: result.stderr.text,
    stdoutTruncated: result.stdout.truncated,
    stderrTruncated: result.stderr.truncated,
  };
}

function runGit({ cwd, args, env, knownSecrets, spawn, stage, recovery, context }) {
  const result = executeGit({ cwd, args, env, knownSecrets, spawn });
  if (result.failedToStart || result.exitCode !== 0) {
    throw new GitSyncError(commandFailureDiagnostic({ stage, result, recovery, context }));
  }
  return result.stdout.text;
}

function assertSafeInput(remote, branch, strategy) {
  if (!remote || !branch) {
    throw new GitSyncError({
      stage: "input",
      exitCode: 2,
      reason: "GIT_REMOTE and GIT_BRANCH must be set explicitly",
      recovery: "set both environment variables and retry",
    });
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9._/-]*$/.test(remote) || remote.includes("..")) {
    throw new GitSyncError({
      stage: "input",
      exitCode: 2,
      reason: "GIT_REMOTE is invalid",
      recovery: "use the configured remote name",
    });
  }
  if (!STRATEGIES.has(strategy)) {
    throw new GitSyncError({
      stage: "input",
      exitCode: 2,
      reason: "unsupported sync strategy",
      recovery: "use ff-only, merge, or rebase",
    });
  }
}

function assertNoInProgressOperation({ cwd, env, knownSecrets, spawn, context }) {
  for (const marker of ["MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-apply", "rebase-merge"]) {
    const markerPath = runGit({
      cwd,
      args: ["rev-parse", "--git-path", marker],
      env,
      knownSecrets,
      spawn,
      context,
      stage: "preflight",
      recovery: "finish or abort the existing Git operation",
    }).trim();
    const resolvedMarker = path.isAbsolute(markerPath) ? markerPath : path.resolve(cwd, markerPath);
    if (fs.existsSync(resolvedMarker)) {
      throw new GitSyncError({
        stage: "preflight",
        exitCode: 2,
        reason: "a Git operation is already in progress",
        recovery: "finish or abort the existing Git operation",
      });
    }
  }
}

function assertCleanWorktree({ cwd, env, knownSecrets, spawn, stage, context }) {
  const status = runGit({
    cwd,
    args: ["status", "--porcelain=v1", "--untracked-files=all"],
    env,
    knownSecrets,
    spawn,
    context,
    stage,
    recovery: "inspect the repository without printing credentials",
  });
  if (status.trim()) {
    throw new GitSyncError({
      stage,
      exitCode: 2,
      reason: "worktree is not clean",
      recovery: "commit or remove local changes explicitly; this command never stashes or cleans",
    });
  }
}

function parseAheadBehind(raw) {
  const match = raw.trim().match(/^(\d+)\s+(\d+)$/);
  if (!match) {
    throw new GitSyncError({
      stage: "compare",
      exitCode: 1,
      reason: "unable to parse ahead/behind counts",
      recovery: "inspect repository refs",
    });
  }
  return { ahead: Number(match[1]), behind: Number(match[2]) };
}

export function runSourceSync({
  cwd = process.cwd(),
  env = process.env,
  strategy = "ff-only",
  spawn = spawnSync,
  log = (message) => console.log(message),
} = {}) {
  const remote = env.GIT_REMOTE;
  const branch = env.GIT_BRANCH;
  assertSafeInput(remote, branch, strategy);

  const context = { remote, branch, strategy };
  const common = { cwd, env, knownSecrets: [], spawn, context };
  runGit({
    ...common,
    args: ["rev-parse", "--is-inside-work-tree"],
    stage: "preflight",
    recovery: "run the command inside a Git worktree",
  });
  runGit({
    ...common,
    args: ["check-ref-format", "--branch", branch],
    stage: "preflight",
    recovery: "set GIT_BRANCH to a valid branch name",
  });
  const currentBranch = runGit({
    ...common,
    args: ["symbolic-ref", "--quiet", "--short", "HEAD"],
    stage: "preflight",
    recovery: "check out the named GIT_BRANCH; detached HEAD is not supported",
  }).trim();
  if (currentBranch !== branch) {
    throw new GitSyncError({
      stage: "preflight",
      exitCode: 2,
      reason: "current branch does not match GIT_BRANCH",
      recovery: "check out the requested branch explicitly",
    });
  }
  assertNoInProgressOperation(common);
  assertCleanWorktree({ ...common, stage: "preflight" });

  const knownSecrets = discoverRemoteKnownSecrets({ cwd, remote, env, spawn });
  const secured = { cwd, env, knownSecrets, spawn, context };
  runGit({
    ...secured,
    args: ["fetch", "--no-tags", "--no-prune", remote, branch],
    stage: "fetch",
    recovery: "verify remote access and retry; credentials are never printed",
  });
  assertNoInProgressOperation(secured);
  assertCleanWorktree({ ...secured, stage: "post-fetch" });

  const oldHead = runGit({
    ...secured,
    args: ["rev-parse", "HEAD"],
    stage: "compare",
    recovery: "inspect repository HEAD",
  }).trim();
  const counts = parseAheadBehind(
    runGit({
      ...secured,
      args: ["rev-list", "--left-right", "--count", "HEAD...FETCH_HEAD"],
      stage: "compare",
      recovery: "inspect fetched refs",
    }),
  );

  let action = "noop";
  if (counts.behind > 0 && counts.ahead === 0) {
    action = "fast-forward";
    runGit({
      ...secured,
      args: ["merge", "--ff-only", "FETCH_HEAD"],
      stage: "integrate",
      recovery: "inspect the fetched branch and retry",
    });
  } else if (counts.behind > 0 && counts.ahead > 0) {
    if (strategy === "ff-only") {
      throw new GitSyncError({
        stage: "integrate",
        exitCode: 3,
        reason: "local and remote histories have diverged",
        recovery: "retry explicitly with update-merge or update-rebase",
        ahead: counts.ahead,
        behind: counts.behind,
      });
    }
    action = strategy;
    const args = strategy === "merge" ? ["merge", "--no-edit", "FETCH_HEAD"] : ["rebase", "FETCH_HEAD"];
    runGit({
      ...secured,
      args,
      stage: "integrate",
      recovery: `resolve the conflict or run git ${strategy} --abort`,
    });
  }

  const newHead = runGit({
    ...secured,
    args: ["rev-parse", "HEAD"],
    stage: "verify",
    recovery: "inspect repository HEAD",
  }).trim();
  assertNoInProgressOperation(secured);
  assertCleanWorktree({ ...secured, stage: "verify" });

  const result = {
    schemaVersion: 1,
    strategy,
    action,
    remote,
    branch,
    ahead: counts.ahead,
    behind: counts.behind,
    oldHead,
    newHead,
  };
  log(JSON.stringify(result));
  return result;
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  try {
    runSourceSync({ strategy: process.argv[2] ?? "ff-only" });
  } catch (error) {
    if (error instanceof GitSyncError || error instanceof RemoteCredentialDiscoveryError) {
      console.error(JSON.stringify(error.diagnostic));
      process.exitCode = error.exitCode || 1;
    } else {
      console.error(
        JSON.stringify({
          stage: "internal",
          exitCode: 1,
          reason: sanitizeGitOutput(error instanceof Error ? error.message : String(error)),
          recovery: "inspect the local runner implementation",
        }),
      );
      process.exitCode = 1;
    }
  }
}
