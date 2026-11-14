/**
 * netbsd-account/plugin.test.ts
 */

import { describe, it, expect, beforeEach, vi } from "vitest";
import { installMockHost } from "../mock-host";

import * as pluginModule from "./plugin";
const plugin = pluginModule as unknown as {
  name: string;
  os: string;
  isolation: string;
  binaries: string[];
  options: { flag: string }[];
  validate: () => void;
  provision: (spec: { workspace_id: string; instance_id: string; work_dir: string }) => { native_id: string; work_dir: string; username: string; privilege: string; home: string };
  destroy: (h: { native_id: string; home: string }) => void;
  destroyByID: (id: string) => void;
  recover: (id: string, workDir: string) => { native_id: string; home: string; username: string; work_dir: string };
  cleanupOrphans: (keep: string[]) => number;
  execArgs: (h: { username: string; home: string }, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[];
};

const hostOptions = {
  "escalation-command": "sudo",
  "account-home-base": "/home",
  "account-shell": "/sbin/nologin",
  "account-login-class": "",
};

const workspaceID = "ws-0123456789ab";
const account = "ac-abc123456789";
const home = "/home/" + account;

function mockRun(captured: string[][], result?: { stdout?: string; stderr?: string; exit_code?: number }) {
  return (argv: string[]) => {
    captured.push(argv);
    return { stdout: result?.stdout ?? "", stderr: result?.stderr ?? "", exit_code: result?.exit_code ?? 0 };
  };
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("netbsd-account plugin", () => {
  it("has correct metadata", () => {
    expect(plugin.name).toBe("netbsd-account");
    expect(plugin.os).toBe("netbsd");
    expect(plugin.isolation).toBe("process");
    expect(plugin.binaries).toContain("sudo");
    expect(plugin.binaries).toContain("doas");
    expect(plugin.binaries).toContain("useradd");
    expect(plugin.binaries).toContain("userdel");
    expect(plugin.binaries).toContain("groupadd");
    expect(plugin.binaries).toContain("groupdel");
    expect(plugin.binaries).toContain("userinfo");
  });

  it("declares the account and escalation options for --help", () => {
    const flags = plugin.options.map((o) => o.flag);
    expect(flags).toContain("account-home-base");
    expect(flags).toContain("account-login-class");
    expect(flags).toContain("account-group");
    expect(flags).toContain("account-shell");
    expect(flags).toContain("escalation-command");
  });

  it("drops to the workspace account and remaps /workspace onto home", () => {
    installMockHost({ options: hostOptions });
    const argv = plugin.execArgs(
      { username: account, home },
      ["true"],
      "/workspace/repo",
      [],
      { stdin: false, root: false },
    );
    expect(argv[0]).toBe("sudo");
    expect(argv[1]).toBe("-u");
    expect(argv[2]).toBe(account);
    expect(argv[3]).toBe("sh");
    expect(argv[4]).toBe("-c");
    const payload = argv[5];
    expect(payload).toContain("cd /home/" + account + "/repo &&");
    expect(payload).toContain("PATH=");
    expect(payload).toContain("HOME=" + home);
    expect(payload).toContain("true");
  });

  it("remaps path arguments for file I/O (cat/cp/chmod)", () => {
    installMockHost({ options: hostOptions });
    const argv = plugin.execArgs(
      { username: account, home },
      ["cat", "/workspace/repo/foo.txt"],
      "",
      [],
      { stdin: false, root: true },
    );
    const payload = argv[5];
    expect(payload).toContain(home + "/repo/foo.txt");
    expect(payload).not.toContain("/workspace/repo/foo.txt");
  });

  it("ignores opts.root and never escalates the workspace to root", () => {
    installMockHost({ options: hostOptions });
    const asUser = plugin.execArgs({ username: account, home }, ["id"], "/workspace/repo", [], { stdin: false, root: false });
    const asRoot = plugin.execArgs({ username: account, home }, ["id"], "/workspace/repo", [], { stdin: false, root: true });
    expect(asUser).toEqual(asRoot);
    expect(asRoot[2]).toBe(account);
  });

  it("provisions a group and account with expected argv", () => {
    const runs: string[][] = [];
    installMockHost({ options: hostOptions, run: mockRun(runs) });

    const handle = plugin.provision({ workspace_id: workspaceID, instance_id: account, work_dir: "/workspace/repo" });

    expect(handle.native_id).toBe(account);
    expect(handle.username).toBe(account);
    expect(handle.privilege).toBe("user");
    expect(handle.home).toBe(home);
    expect(handle.work_dir).toBe(home + "/repo");

    expect(runs[0]).toEqual(["sudo", "groupadd", account]);
    expect(runs[1]).toEqual([
      "sudo", "useradd", "-d", home, "-m", "-M", "700", "-g", account, "-G", "wsd", "-s", "/sbin/nologin", account,
    ]);
    expect(runs[2]).toEqual(["sudo", "chown", "-R", account + ":" + account, home]);
    expect(handle.native_id).not.toBe(workspaceID);
    expect(handle.home).not.toContain(workspaceID);
  });

  it("requires core to provide an opaque instance id", () => {
    installMockHost({ options: hostOptions, run: mockRun([]) });
    expect(() => plugin.provision({ workspace_id: workspaceID, instance_id: "", work_dir: "/workspace/repo" })).toThrow(/instance_id/);
  });

  it("appends -L when a login class is configured", () => {
    const runs: string[][] = [];
    installMockHost({
      options: { ...hostOptions, "account-login-class": "daemon" },
      run: mockRun(runs),
    });
    plugin.provision({ workspace_id: workspaceID, instance_id: account, work_dir: "/workspace/repo" });
    expect(runs[1]).toEqual([
      "sudo", "useradd", "-d", home, "-m", "-M", "700", "-g", account, "-G", "wsd", "-s", "/sbin/nologin",
      "-L", "daemon", account,
    ]);
  });

  it("validates that the escalator reaches root", () => {
    const runs: string[][] = [];
    installMockHost({ options: hostOptions, run: mockRun(runs) });
    expect(() => plugin.validate()).not.toThrow();
    expect(runs[0]).toEqual(["sudo", "test", "-r", "/etc/master.passwd"]);
    expect(runs[1]).toEqual(["grep", "-q", "^wsd:", "/etc/group"]);
  });

  it("rejects an empty escalator", () => {
    installMockHost({ options: { ...hostOptions, "escalation-command": "" }, run: mockRun([]) });
    expect(() => plugin.validate()).toThrow(/requires --escalation-command/);
  });

  it("checks the login class exists in /etc/login.conf", () => {
    const runs: string[][] = [];
    installMockHost({
      options: { ...hostOptions, "account-login-class": "daemon" },
      run: mockRun(runs),
    });
    expect(() => plugin.validate()).not.toThrow();
    expect(runs[1]).toEqual(["grep", "-q", "^daemon:", "/etc/login.conf"]);
    expect(runs[2]).toEqual(["grep", "-q", "^wsd:", "/etc/group"]);
  });

  it("destroys an account and tolerates a missing account", () => {
    const runs: string[][] = [];
    installMockHost({ options: hostOptions, run: mockRun(runs) });
    plugin.destroy({ native_id: account, home });
    expect(runs[0]).toEqual(["sudo", "userdel", "-r", account]);
    expect(runs[1]).toEqual(["sudo", "groupdel", account]);
    expect(runs[2]).toEqual(["sudo", "rm", "-rf", home]);
  });

  it("tolerates a benign userdel error but throws on a real one", () => {
    installMockHost({
      options: hostOptions,
      run: (argv: string[]) =>
        argv[1] === "userdel"
          ? { stdout: "", stderr: "userdel: No such user `" + account + "'", exit_code: 1 }
          : { stdout: "", stderr: "", exit_code: 0 },
    });
    expect(() => plugin.destroy({ native_id: account, home })).not.toThrow();

    installMockHost({
      options: hostOptions,
      run: (argv: string[]) =>
        argv[1] === "userdel"
          ? { stdout: "", stderr: "userdel: permission denied", exit_code: 1 }
          : { stdout: "", stderr: "", exit_code: 0 },
    });
    expect(() => plugin.destroy({ native_id: account, home })).toThrow(/failed to remove account/);
  });

  it("recovers an existing account handle", () => {
    installMockHost({ options: hostOptions, run: mockRun([]) });
    const handle = plugin.recover(account, "/workspace/repo");
    expect(handle.native_id).toBe(account);
    expect(handle.home).toBe(home);
    expect(handle.username).toBe(account);
    expect(handle.work_dir).toBe(home + "/repo");
  });

  it("throws when recovering a missing account", () => {
    installMockHost({
      options: hostOptions,
      run: () => ({ stdout: "", stderr: "userinfo: user " + account + " does not exist", exit_code: 1 }),
    });
    expect(() => plugin.recover(account, "/workspace/repo")).toThrow(/not found/);
  });

  it("cleans up orphan workspace accounts but leaves others alone", () => {
    const passwd = [
      "root:x:0:0:Charlie &:/root:/bin/sh",
      account + ":x:1000:1000::" + home + ":/sbin/nologin",
      "ws-fffffffff:x:1001:1001::/home/ws-fffffffff:/sbin/nologin",
      "ac-ffffffffffff:x:1002:1002::/home/ac-ffffffffffff:/sbin/nologin",
      "someone:x:1003:1003::/home/someone:/bin/sh",
    ].join("\n");

    const runs: string[][] = [];
    installMockHost({
      options: hostOptions,
      run: (argv: string[]) => {
        runs.push(argv);
        if (argv[0] === "cat") return { stdout: passwd, stderr: "", exit_code: 0 };
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });

    const removed = plugin.cleanupOrphans([account]);
    expect(removed).toBe(2);
    expect(runs[0]).toEqual(["cat", "/etc/passwd"]);
    // The current account is kept; both a legacy ws- orphan and a new ac-
    // orphan are destroyed, and the unrelated "someone" account is untouched.
    expect(runs[1]).toEqual(["sudo", "userdel", "-r", "ws-fffffffff"]);
    expect(runs[4]).toEqual(["sudo", "userdel", "-r", "ac-ffffffffffff"]);
  });

  it("no longer exports destroyWorkspaceByID", () => {
    const p = pluginModule as unknown as { destroyWorkspaceByID?: unknown };
    expect(p.destroyWorkspaceByID).toBeUndefined();
  });
});
