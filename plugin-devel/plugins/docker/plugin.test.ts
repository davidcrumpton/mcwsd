/**
 * docker/plugin.test.ts
 */

import { describe, it, expect, beforeEach, vi } from "vitest";
import { installMockHost } from "../mock-host";

import * as pluginModule from "./plugin";
const plugin = pluginModule as unknown as {
  name: string;
  binaries: string[];
  options: { flag: string }[];
  execArgs: (h: unknown, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[];
  setNetwork: (h: { native_id: string }, enabled: boolean) => void;
  bootstrapCmd: () => string[] | null;
};

beforeEach(() => {
  vi.clearAllMocks();
});

describe("docker plugin", () => {
  it("declares the docker-network option for --help", () => {
    expect(plugin.options.map((o) => o.flag)).toContain("docker-network");
  });

  it("maps a plain exec to argv", () => {
    installMockHost();
    const argv = plugin.execArgs({ native_id: "abc" }, ["true"], "/w", [], { stdin: false, root: false });
    expect(argv).toEqual(["docker", "exec", "--workdir", "/w", "abc", "true"]);
  });

  it("connects and disconnects the configured network", () => {
    const runs: string[][] = [];
    const host = installMockHost({
      options: { "docker-network": "mybridge" },
      run: (argv: string[]) => {
        runs.push(argv);
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    expect(host).toBeTruthy();

    plugin.setNetwork({ native_id: "abc" }, true);
    plugin.setNetwork({ native_id: "abc" }, false);

    expect(runs[0]).toEqual(["docker", "network", "connect", "mybridge", "abc"]);
    expect(runs[1]).toEqual(["docker", "network", "disconnect", "mybridge", "abc"]);
  });

  it("tolerates an already-attached connect error", () => {
    installMockHost({
      run: () => ({ stdout: "", stderr: "endpoint with name abc already exists in network bridge", exit_code: 1 }),
    });
    expect(() => plugin.setNetwork({ native_id: "abc" }, true)).not.toThrow();
  });

  it("returns the git safe.directory bootstrap", () => {
    installMockHost();
    expect(plugin.bootstrapCmd()).toEqual(["git", "config", "--system", "--add", "safe.directory", "*"]);
  });
});
