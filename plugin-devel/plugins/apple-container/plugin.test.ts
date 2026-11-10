/**
 * apple-container/plugin.test.ts
 */

import { describe, it, expect, beforeEach, vi } from "vitest";
import { installMockHost } from "../mock-host";

import * as pluginModule from "./plugin";
const plugin = pluginModule as unknown as {
  name: string;
  description: string;
  os: string;
  isolation: string;
  binaries: string[];
  options: { flag: string; type: string; default?: string }[];
  provision: (spec: Record<string, unknown>) => Record<string, unknown>;
  destroy: (h: { native_id: string }) => void;
  destroyByID: (id: string) => void;
  recover: (id: string, workDir: string) => Record<string, unknown>;
  setNetwork: (h: { native_id: string }, enabled: boolean) => void;
  execArgs: (h: { native_id: string }, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[];
  bootstrapCmd: () => string[] | null;
};

interface RunCall {
  argv: string[];
}

beforeEach(() => {
  vi.clearAllMocks();
});

function captureHost(runs: RunCall[], options: Record<string, unknown> = {}) {
  return installMockHost({
    options,
    run: (argv: string[]) => {
      runs.push({ argv });
      return { stdout: "", stderr: "", exit_code: 0 };
    },
  });
}

describe("apple container plugin", () => {
  it("declares its metadata and network option", () => {
    expect(plugin.name).toBe("apple-container");
    expect(plugin.os).toBe("linux");
    expect(plugin.isolation).toBe("container");
    expect(plugin.binaries).toEqual(["container", "mkdir"]);
    expect(plugin.options.map((o) => o.flag)).toContain("container-network");
  });

  it("pulls, creates, and starts a container with the workspace mounts", () => {
    const runs: RunCall[] = [];
    const mock = captureHost(runs, { "container-network": "default" });

    const handle = plugin.provision({
      workspace_id: "ws-123",
      image: "ubuntu:latest",
      work_dir: "/workspace/repo",
      cache_mount_path: "/var/cache/wsd",
      workspace_base_path: "/home/u/wsd",
    });

    expect(handle).toEqual({
      native_id: "wsd-ws-123",
      work_dir: "/workspace/repo",
      username: "root",
      privilege: "root",
    });

    expect(runs[0].argv).toEqual(["container", "image", "pull", "ubuntu:latest"]);
    expect(runs[1].argv).toEqual(["mkdir", "-p", "/home/u/wsd/ws-123"]);
    expect(runs[2].argv).toEqual([
      "container",
      "create",
      "--name",
      "wsd-ws-123",
      "--workdir",
      "/workspace/repo",
      "--network",
      "default",
      "--volume",
      "/var/cache/wsd:/cache",
      "--volume",
      "/home/u/wsd/ws-123:/workspace/repo",
      "ubuntu:latest",
      "sleep",
      "infinity",
    ]);
    expect(runs[3].argv).toEqual(["container", "start", "wsd-ws-123"]);
    expect(mock.onFail).toHaveBeenCalledWith(["container", "delete", "--force", "wsd-ws-123"]);
  });

  it("honours container-network none and omits optional mounts", () => {
    const runs: RunCall[] = [];
    captureHost(runs, { "container-network": "none" });

    plugin.provision({
      workspace_id: "ws-1",
      image: "alpine",
      work_dir: "/workspace",
      cache_mount_path: "",
      workspace_base_path: "",
    });

    expect(runs[0].argv).toEqual(["container", "image", "pull", "alpine"]);
    expect(runs[1].argv).toEqual([
      "container",
      "create",
      "--name",
      "wsd-ws-1",
      "--workdir",
      "/workspace",
      "--network",
      "none",
      "alpine",
      "sleep",
      "infinity",
    ]);
    expect(runs.length).toBe(3);
  });

  it("fails provision when the image cannot be pulled", () => {
    installMockHost({
      run: () => ({ stdout: "", stderr: "manifest unknown", exit_code: 125 }),
    });
    expect(() =>
      plugin.provision({ workspace_id: "ws-1", image: "missing", work_dir: "/w" }),
    ).toThrow(/failed to pull image/);
  });

  it("deletes containers by handle and by id", () => {
    const runs: RunCall[] = [];
    captureHost(runs);

    plugin.destroy({ native_id: "wsd-ws-1" });
    plugin.destroyByID("wsd-ws-2");

    expect(runs).toEqual([
      { argv: ["container", "delete", "--force", "wsd-ws-1"] },
      { argv: ["container", "delete", "--force", "wsd-ws-2"] },
    ]);
  });

  it("recovers a handle from an existing container", () => {
    installMockHost({
      run: () => ({ stdout: "[]", stderr: "", exit_code: 0 }),
    });

    expect(plugin.recover("wsd-ws-1", "/w")).toEqual({
      native_id: "wsd-ws-1",
      work_dir: "/w",
      username: "root",
      privilege: "root",
    });
  });

  it("throws when recovering a missing container", () => {
    installMockHost({
      run: () => ({ stdout: "", stderr: "container not found", exit_code: 1 }),
    });

    expect(() => plugin.recover("gone", "/w")).toThrow(/not found/);
  });

  it("leaves the network alone because the attachment is fixed at create", () => {
    const runs: RunCall[] = [];
    captureHost(runs);

    plugin.setNetwork({ native_id: "wsd-ws-1" }, true);
    plugin.setNetwork({ native_id: "wsd-ws-1" }, false);

    expect(runs).toEqual([]);
  });

  it("maps a plain exec to argv", () => {
    installMockHost();
    const argv = plugin.execArgs({ native_id: "abc123" }, ["ls", "/"], "/w", [], { stdin: false, root: false });
    expect(argv).toEqual(["container", "exec", "--workdir", "/w", "abc123", "ls", "/"]);
  });

  it("adds -i and --user 0 for a privileged stdin file op", () => {
    installMockHost();
    const argv = plugin.execArgs({ native_id: "abc123" }, ["cat"], "", [], { stdin: true, root: true });
    expect(argv).toEqual(["container", "exec", "-i", "--user", "0", "--workdir", "", "abc123", "cat"]);
  });

  it("propagates env and root flag", () => {
    installMockHost();
    const argv = plugin.execArgs({ native_id: "abc123" }, ["true"], "/w", ["A=B"], { stdin: false, root: true });
    expect(argv).toEqual([
      "container",
      "exec",
      "--user",
      "0",
      "--workdir",
      "/w",
      "--env",
      "A=B",
      "abc123",
      "true",
    ]);
  });

  it("adds non-ownership-preserving flags to root tar extraction", () => {
    installMockHost();
    const argv = plugin.execArgs(
      { native_id: "abc123" },
      ["tar", "-xf", "-", "-C", "/workspace"],
      "",
      [],
      { stdin: true, root: true },
    );
    expect(argv).toEqual([
      "container",
      "exec",
      "-i",
      "--user",
      "0",
      "--workdir",
      "",
      "abc123",
      "tar",
      "-o",
      "-m",
      "--no-same-permissions",
      "--strip-components=1",
      "-xf",
      "-",
      "-C",
      "/workspace",
    ]);
  });

  it("leaves user tar commands untouched", () => {
    installMockHost();
    const argv = plugin.execArgs(
      { native_id: "abc123" },
      ["tar", "-xf", "archive.tar"],
      "/w",
      [],
      { stdin: false, root: false },
    );
    expect(argv).toEqual([
      "container",
      "exec",
      "--workdir",
      "/w",
      "abc123",
      "tar",
      "-xf",
      "archive.tar",
    ]);
  });

  it("bootstrap marks bind-mounted repos as safe for git", () => {
    expect(plugin.bootstrapCmd()).toEqual(["git", "config", "--system", "--add", "safe.directory", "*"]);
  });
});
