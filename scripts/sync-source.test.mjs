import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { afterEach, describe, test } from "node:test";
import { fileURLToPath } from "node:url";

import {
  GitSyncError,
  MAX_SAFE_STREAM_CHARS,
  RemoteCredentialDiscoveryError,
  discoverRemoteKnownSecrets,
  extractRemoteKnownSecrets,
  runSourceSync,
  safeStream,
  sanitizeGitOutput,
} from "./sync-source.mjs";

const scriptPath = fileURLToPath(new URL("./sync-source.mjs", import.meta.url));
const fixtureDirs = [];

function git(cwd, ...args) {
  const result = spawnSync("git", args, {
    cwd,
    encoding: "utf8",
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "QIA-319 Test",
      GIT_AUTHOR_EMAIL: "qia319@example.invalid",
      GIT_COMMITTER_NAME: "QIA-319 Test",
      GIT_COMMITTER_EMAIL: "qia319@example.invalid",
      GIT_TERMINAL_PROMPT: "0",
    },
  });
  if (result.status !== 0) {
    throw new Error(`git ${args.join(" ")} failed: ${result.stderr || result.stdout}`);
  }
  return result.stdout.trim();
}

function commit(repo, filename, contents, message) {
  fs.writeFileSync(path.join(repo, filename), contents);
  git(repo, "add", filename);
  git(repo, "commit", "-m", message);
  return git(repo, "rev-parse", "HEAD");
}

function createRepo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "qia319-sync-"));
  fixtureDirs.push(root);
  const origin = path.join(root, "origin.git");
  const seed = path.join(root, "seed");
  const local = path.join(root, "local");
  git(root, "init", "--bare", origin);
  fs.mkdirSync(seed);
  git(seed, "init", "--initial-branch=main");
  commit(seed, "shared.txt", "base\n", "base");
  git(seed, "remote", "add", "origin", origin);
  git(seed, "push", "-u", "origin", "main");
  git(origin, "symbolic-ref", "HEAD", "refs/heads/main");
  git(root, "clone", origin, local);
  git(local, "config", "user.name", "QIA-319 Test");
  git(local, "config", "user.email", "qia319@example.invalid");
  return { root, origin, seed, local };
}

function syncEnv(extra = {}) {
  return { ...process.env, GIT_REMOTE: "origin", GIT_BRANCH: "main", ...extra };
}

function runSync(cwd, strategy = "ff-only", options = {}) {
  const messages = [];
  const result = runSourceSync({
    cwd,
    strategy,
    env: options.env ?? syncEnv(),
    spawn: options.spawn ?? spawnSync,
    log: (message) => messages.push(message),
  });
  return { result, messages };
}

afterEach(() => {
  for (const fixture of fixtureDirs.splice(0)) {
    fs.rmSync(fixture, { force: true, recursive: true });
  }
});

describe("Git diagnostic sanitization", () => {
  test("redacts discovered variants and the full inline matrix before emission", () => {
    const known = ["raw+pass", "query token"];
    const raw = [
      "fatal: https://alice:raw%2Bpass@example.invalid/repo.git?client_secret=query+token",
      "Authorization: Bearer github_pat_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
      'client_secret="value with spaces and \\"quotes\\"" retry=disabled',
      "widget_token='single quoted value' next=safe",
      "x-api-key=bare-value, retry=yes",
      'password="unterminated credential',
      "credential='another unterminated value",
      "/tmp/client_secret=fixture.txt",
    ].join("\n");

    const sanitized = sanitizeGitOutput(raw, known);
    for (const secret of [
      "alice",
      "raw+pass",
      "raw%2Bpass",
      "query token",
      "query+token",
      "value with spaces",
      "single quoted value",
      "bare-value",
      "unterminated credential",
      "another unterminated value",
      "github_pat_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    ]) {
      assert.equal(sanitized.includes(secret), false, `leaked ${secret}`);
    }
    assert.match(sanitized, /client_secret="\[REDACTED\]" retry=disabled/);
    assert.match(sanitized, /widget_token='\[REDACTED\]' next=safe/);
    assert.match(sanitized, /x-api-key=\[REDACTED\], retry=yes/);
    assert.match(sanitized, /password="\[REDACTED\]"/);
    assert.match(sanitized, /credential='\[REDACTED\]'/);
    assert.match(sanitized, /\/tmp\/client_secret=fixture\.txt/);
  });

  test("redacts raw, percent and form inline values with no discovered secrets", () => {
    const samples = [
      ["fatal: remote rejected client_secret=raw-secret trace=kept", "raw-secret", "trace=kept"],
      ["warning: auth failed oauth_client_secret: percent%2Bsecret stage=fetch", "percent%2Bsecret", "stage=fetch"],
      ["error: forbidden x-api-key=form+secret retry=yes", "form+secret", "retry=yes"],
    ];
    for (const [raw, secret, safeSuffix] of samples) {
      const sanitized = sanitizeGitOutput(raw, []);
      assert.equal(sanitized.includes(secret), false);
      assert.match(sanitized, /\[REDACTED\]/);
      assert.equal(sanitized.includes(safeSuffix), true);
    }
  });

  test("redacts before applying the deterministic 64 KiB bound", () => {
    const secret = "secret-near-truncation-boundary";
    const raw = `${"a".repeat(70_000)} client_secret=${secret} ${"z".repeat(70_000)}`;
    const result = safeStream(raw, [secret]);
    assert.equal(result.truncated, true);
    assert.equal(result.text.length, MAX_SAFE_STREAM_CHARS);
    assert.equal(result.text.includes(secret), false);
    assert.match(result.text, /SAFE OUTPUT TRUNCATED/);
  });

  test("extracts raw, percent and form variants in stable discovery order", () => {
    const { local } = createRepo();
    const remoteUrl =
      "https://alice:raw%2Bpass@example.invalid/repo.git?client_secret=query+token&widget_token=query%2Btoken&safe=value";
    git(local, "remote", "set-url", "origin", remoteUrl);

    const first = discoverRemoteKnownSecrets({ cwd: local, remote: "origin" });
    const second = discoverRemoteKnownSecrets({ cwd: local, remote: "origin" });
    assert.deepEqual(first, second);
    assert.deepEqual(first, [...new Set(first)]);
    assert.deepEqual(first, [...first].sort((left, right) => right.length - left.length || left.localeCompare(right)));
    for (const value of ["alice", "raw+pass", "raw%2Bpass", "query token", "query+token", "query%2Btoken"]) {
      assert.equal(first.includes(value), true, `missing ${value}`);
    }
  });

  test("treats scp user as a secret and local paths as credential-free", () => {
    assert.equal(extractRemoteKnownSecrets("git@example.invalid:team/repo.git").has("git"), true);
    assert.equal(extractRemoteKnownSecrets("../origin.git").size, 0);
  });
});

describe("remote credential discovery fail-closed boundary", () => {
  test("rejects malformed authorities before any fetch invocation", () => {
    const { local } = createRepo();
    const malformed = [
      "https://user:pass@/repo.git",
      "https://user:pass@example.invalid:not-a-port/repo.git",
      "https://user:pass@example.invalid:70000/repo.git",
      "https://user:pass@[::1/repo.git",
      "https://user:pass%ZZ@example.invalid/repo.git",
    ];

    for (const remoteUrl of malformed) {
      git(local, "remote", "set-url", "origin", remoteUrl);
      let fetchInvocations = 0;
      const observedSpawn = (command, args, options) => {
        if (command === "git" && args[0] === "fetch") fetchInvocations += 1;
        return spawnSync(command, args, options);
      };
      assert.throws(
        () => runSync(local, "ff-only", { spawn: observedSpawn }),
        (error) => error instanceof RemoteCredentialDiscoveryError,
      );
      assert.equal(fetchInvocations, 0, remoteUrl);
    }
    assert.throws(() => extractRemoteKnownSecrets("https://user:pass@example.invalid:0/repo.git"));
  });

  test("an actual credential-bearing fetch failure leaks no discovered value to CLI sinks", () => {
    const { local } = createRepo();
    const remoteUrl =
      "https://alice:raw%2Bpass@127.0.0.1:1/repo.git?client_secret=query+token&widget_token=aux%2Btoken";
    git(local, "remote", "set-url", "origin", remoteUrl);
    const knownSecrets = discoverRemoteKnownSecrets({ cwd: local, remote: "origin" });

    const messages = [];
    let directError;
    try {
      runSourceSync({
        cwd: local,
        strategy: "ff-only",
        env: syncEnv(),
        log: (message) => messages.push(message),
      });
    } catch (error) {
      directError = error;
    }
    assert.ok(directError instanceof GitSyncError);
    assert.equal(directError.diagnostic.remote, "origin");
    assert.equal(directError.diagnostic.strategy, "ff-only");
    assert.match(directError.diagnostic.stderr, /127\.0\.0\.1/);

    const child = spawnSync(process.execPath, [scriptPath, "ff-only"], {
      cwd: local,
      encoding: "utf8",
      env: syncEnv(),
      timeout: 15_000,
    });
    assert.notEqual(child.status, 0);
    const emitted = `${child.stdout}${child.stderr}${directError.message}${JSON.stringify(directError.diagnostic)}${messages.join("\n")}`;
    for (const secret of knownSecrets) {
      assert.equal(emitted.includes(secret), false, `CLI leaked a discovered variant`);
    }
    assert.equal(emitted.includes("raw%2Bpass"), false);
    assert.equal(emitted.includes("query+token"), false);
    assert.match(emitted, /"stage":"fetch"/);
    assert.match(emitted, /\[REDACTED\]/);
  });
});

describe("real Git synchronization", () => {
  test("fast-forwards a clean branch and emits bounded provenance", () => {
    const { seed, local } = createRepo();
    const remoteHead = commit(seed, "remote.txt", "remote\n", "remote");
    git(seed, "push", "origin", "main");

    const { result, messages } = runSync(local);
    assert.equal(result.action, "fast-forward");
    assert.equal(result.newHead, remoteHead);
    assert.equal(git(local, "rev-parse", "HEAD"), remoteHead);
    assert.equal(messages.length, 1);
    assert.deepEqual(JSON.parse(messages[0]), result);
  });

  test("rejects dirty and detached worktrees without fetching", () => {
    const { local } = createRepo();
    fs.writeFileSync(path.join(local, "untracked.txt"), "dirty\n");
    let fetchInvocations = 0;
    const observedSpawn = (command, args, options) => {
      if (command === "git" && args[0] === "fetch") fetchInvocations += 1;
      return spawnSync(command, args, options);
    };
    assert.throws(
      () => runSync(local, "ff-only", { spawn: observedSpawn }),
      (error) => error instanceof GitSyncError && error.diagnostic.reason === "worktree is not clean",
    );
    assert.equal(fetchInvocations, 0);

    fs.rmSync(path.join(local, "untracked.txt"));
    git(local, "checkout", "--detach");
    assert.throws(
      () => runSync(local),
      (error) => error instanceof GitSyncError && error.diagnostic.stage === "preflight",
    );
  });

  test("rejects illegal remote/ref inputs before Git mutation", () => {
    const { local } = createRepo();
    assert.throws(
      () => runSync(local, "ff-only", { env: syncEnv({ GIT_REMOTE: "../origin" }) }),
      (error) => error instanceof GitSyncError && error.diagnostic.stage === "input",
    );
    assert.throws(
      () => runSync(local, "ff-only", { env: syncEnv({ GIT_BRANCH: "bad ref" }) }),
      (error) => error instanceof GitSyncError && error.diagnostic.stage === "preflight",
    );
  });

  test("ff-only rejects divergence without moving HEAD", () => {
    const { seed, local } = createRepo();
    const localHead = commit(local, "local.txt", "local\n", "local");
    commit(seed, "remote.txt", "remote\n", "remote");
    git(seed, "push", "origin", "main");

    assert.throws(
      () => runSync(local),
      (error) => error instanceof GitSyncError && error.diagnostic.reason.includes("diverged"),
    );
    assert.equal(git(local, "rev-parse", "HEAD"), localHead);
  });

  test("explicit merge integrates divergent histories", () => {
    const { seed, local } = createRepo();
    commit(local, "local.txt", "local\n", "local");
    commit(seed, "remote.txt", "remote\n", "remote");
    git(seed, "push", "origin", "main");

    const { result } = runSync(local, "merge");
    assert.equal(result.action, "merge");
    assert.equal(git(local, "rev-list", "--parents", "-n", "1", "HEAD").split(/\s+/).length, 3);
  });

  test("explicit rebase integrates divergent histories without a merge commit", () => {
    const { seed, local } = createRepo();
    commit(local, "local.txt", "local\n", "local");
    const remoteHead = commit(seed, "remote.txt", "remote\n", "remote");
    git(seed, "push", "origin", "main");

    const { result } = runSync(local, "rebase");
    assert.equal(result.action, "rebase");
    assert.equal(git(local, "merge-base", "--is-ancestor", remoteHead, "HEAD"), "");
    assert.equal(git(local, "rev-list", "--parents", "-n", "1", "HEAD").split(/\s+/).length, 2);
  });

  test("leaves a real merge conflict for explicit operator recovery", () => {
    const { seed, local } = createRepo();
    commit(local, "shared.txt", "local conflict\n", "local conflict");
    commit(seed, "shared.txt", "remote conflict\n", "remote conflict");
    git(seed, "push", "origin", "main");

    assert.throws(
      () => runSync(local, "merge"),
      (error) => error instanceof GitSyncError && error.diagnostic.stage === "integrate",
    );
    const mergeHeadPath = git(local, "rev-parse", "--git-path", "MERGE_HEAD");
    assert.equal(fs.existsSync(path.resolve(local, mergeHeadPath)), true);
  });
});
