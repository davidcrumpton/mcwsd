/**
 * podman/plugin.test.ts
 */

import { describe, it, expect, beforeEach, vi } from "vitest";
import { installMockHost } from "../mock-host";

import * as pluginModule from "./plugin";
const plugin = pluginModule as unknown as {
  name: string;
  binaries: string[];
  execArgs: (h: unknown, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[];
};

beforeEach(() => {
  vi.clearAllMocks();
});

describe("podman plugin", () => {
  it("has correct metadata", () => {
    expect(plugin.name).toBe("podman");
    expect(plugin.binaries).toContain("podman");
  });

  it("maps a plain exec to argv", () => {
    installMockHost();
    const handle = { native_id: "abc123" };
    const argv = plugin.execArgs(handle, ["ls", "/"], "/w", [], { stdin: false, root: false });
    expect(argv).toEqual(["podman", "exec", "--workdir", "/w", "abc123", "ls", "/"]);
  });

  it("adds -i and --user 0 for a privileged stdin file op", () => {
    installMockHost();
    const handle = { native_id: "abc123" };
    const argv = plugin.execArgs(handle, ["cat"], "", [], { stdin: true, root: true });
    expect(argv).toEqual(["podman", "exec", "-i", "--user", "0", "--workdir", "", "abc123", "cat"]);
  });

  it("propagates env and root flag", () => {
    installMockHost();
    const handle = { native_id: "abc123" };
    const argv = plugin.execArgs(handle, ["true"], "/w", ["A=B"], { stdin: false, root: true });
    expect(argv).toEqual(["podman", "exec", "--user", "0", "--workdir", "/w", "--env", "A=B", "abc123", "true"]);
  });
});
