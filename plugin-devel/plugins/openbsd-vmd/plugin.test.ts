/**
 * openbsd-vmd/plugin.test.ts
 */

import { describe, it, expect, beforeEach, vi } from "vitest";
import { installMockHost } from "../mock-host";

import * as pluginModule from "./plugin";
const plugin = pluginModule as unknown as {
  name: string;
  os: string;
  isolation: string;
  binaries: string[];
  options: { flag: string; type: string; default?: string }[];
  provision: (spec: Record<string, unknown>) => Record<string, unknown>;
  destroy: (h: { native_id: string }) => void;
  destroyByID: (id: string) => void;
  destroyWorkspaceByID: (id: string) => void;
  recover: (id: string, workDir: string) => Record<string, unknown>;
  cleanupOrphans: (keep: string[]) => number;
  execArgs: (h: Record<string, unknown>, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[];
};

interface RunCall {
  argv: string[];
}

const defaultOptions = {
  "openbsd-vm-root": "/var/wsd/vms/openbsd",
  "openbsd-master-image": "/var/wsd/vms/openbsd/disk-master.qcow2",
  "openbsd-memory": "1G",
  "openbsd-processors": 1,
  "openbsd-login-user": "root",
  "openbsd-ssh-key": "/var/wsd/agent_ed25519",
  "openbsd-ssh-port": 22,
  "openbsd-guest-os": "openbsd",
  "openbsd-escalation-command": "doas",
  "openbsd-guest-escalation-command": "doas",
  "openbsd-boot-timeout": 300,
  "openbsd-ssh-probe-timeout": 20,
};

function captureHost(runs: RunCall[]) {
  return installMockHost({
    options: { ...defaultOptions },
    run: (argv: string[]) => {
      runs.push({ argv });
      const joined = argv.join(" ");
      if (joined.indexOf("vmctl start") !== -1) {
        return { stdout: "vmctl: started vm 6 successfully, tty /dev/ttyp3\n", stderr: "", exit_code: 0 };
      }
      if (joined.indexOf("arp -an") !== -1) {
        return { stdout: "? (100.64.6.3) at 00:00:00:00:00:00 on tap0\n", stderr: "", exit_code: 0 };
      }
      if (joined.indexOf("nc -z") !== -1) {
        return { stdout: "", stderr: "", exit_code: 0 };
      }
      if (argv[0] === "ssh") {
        return { stdout: "", stderr: "", exit_code: 0 };
      }
      return { stdout: "", stderr: "", exit_code: 0 };
    },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("openbsd-vmd plugin", () => {
  it("declares metadata and the vmctl/ssh binaries", () => {
    expect(plugin.name).toBe("openbsd-vmd");
    expect(plugin.os).toBe("openbsd");
    expect(plugin.isolation).toBe("vm");
    expect(plugin.binaries).toContain("vmctl");
    expect(plugin.binaries).toContain("ssh");
    expect(plugin.binaries).toContain("doas");
  });

  it("declares the openbsd-* options", () => {
    const flags = plugin.options.map((o) => o.flag);
    for (const want of ["openbsd-vm-root", "openbsd-master-image", "openbsd-login-user", "openbsd-ssh-key", "openbsd-escalation-command", "openbsd-boot-timeout"]) {
      expect(flags).toContain(want);
    }
  });

  it("maps an exec to an ssh argv with a PATH and cd payload", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = {
      native_id: "wsd-ws-abc",
      ip: "100.64.6.3",
      port: 22,
      username: "root",
      key_path: "/var/wsd/agent_ed25519",
      escalation: "",
    };
    const argv = plugin.execArgs(handle, ["echo", "hi"], "/workspace/repo", [], { stdin: false, root: false });
    expect(argv[0]).toBe("ssh");
    expect(argv).toContain("-o");
    expect(argv).toContain("StrictHostKeyChecking=no");
    expect(argv).toContain("BatchMode=yes");
    expect(argv).toContain("-i");
    expect(argv).toContain("/var/wsd/agent_ed25519");
    expect(argv).toContain("-p");
    expect(argv).toContain("22");
    expect(argv).toContain("root@100.64.6.3");
    const payload = argv[argv.length - 1];
    expect(payload).toContain("cd /workspace/repo && PATH=");
    expect(payload).toContain("echo hi");
  });

  it("wraps the payload in doas sh -c for a non-root login user", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = {
      native_id: "wsd-ws-abc",
      ip: "100.64.6.3",
      port: 22,
      username: "wsd",
      key_path: "/var/wsd/agent_ed25519",
      escalation: "doas",
    };
    const argv = plugin.execArgs(handle, ["echo", "hi"], "/w", [], { stdin: false, root: false });
    const payload = argv[argv.length - 1];
    expect(payload).toContain("doas sh -c");
    expect(payload).toContain("cd /w && PATH=");
    expect(payload).toContain("echo hi");
  });

  it("omits -i when no key is configured", () => {
    installMockHost({ options: { ...defaultOptions, "openbsd-ssh-key": "" } });
    const handle = {
      native_id: "wsd-ws-abc",
      ip: "100.64.6.3",
      port: 22,
      username: "root",
      key_path: "",
      escalation: "",
    };
    const argv = plugin.execArgs(handle, ["true"], "", [], { stdin: false, root: false });
    expect(argv).not.toContain("-i");
  });

  it("provisions a VM: mkdir, clone, start, then waits for SSH", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    const handle = plugin.provision({ workspace_id: "ws-abc", work_dir: "/workspace/repo" });

    expect(handle.native_id).toBe("wsd-ws-abc");
    expect(handle.ip).toBe("100.64.6.3");
    expect(handle.os).toBe("openbsd");

    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("mkdir -p /var/wsd/vms/openbsd/wsd-ws-abc") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("doas vmctl create -b") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("doas vmctl start -L -B disk") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("arp -an") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("nc -z") !== -1)).toBe(true);
    expect(runs.some((r) => r.argv[0] === "ssh")).toBe(true);
  });

  it("adds -p only when processors > 1", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions, "openbsd-processors": 4 },
      run: (argv: string[]) => {
        runs.push({ argv });
        const joined = argv.join(" ");
        if (joined.indexOf("vmctl start") !== -1) {
          return { stdout: "vmctl: started vm 6 successfully, tty /dev/ttyp3\n", stderr: "", exit_code: 0 };
        }
        if (joined.indexOf("arp -an") !== -1) {
          return { stdout: "? (100.64.6.3) at 00:00:00:00:00:00\n", stderr: "", exit_code: 0 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    plugin.provision({ workspace_id: "ws-abc", work_dir: "/w" });
    const start = runs.find((r) => r.argv.join(" ").indexOf("vmctl start") !== -1)!;
    expect(start.argv).toContain("-p");
    expect(start.argv).toContain("4");
  });

  it("does not add -p when processors is 1", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions, "openbsd-processors": 1 },
      run: (argv: string[]) => {
        runs.push({ argv });
        const joined = argv.join(" ");
        if (joined.indexOf("vmctl start") !== -1) {
          return { stdout: "vmctl: started vm 6 successfully, tty /dev/ttyp3\n", stderr: "", exit_code: 0 };
        }
        if (joined.indexOf("arp -an") !== -1) {
          return { stdout: "? (100.64.6.3) at 00:00:00:00:00:00\n", stderr: "", exit_code: 0 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    plugin.provision({ workspace_id: "ws-abc", work_dir: "/w" });
    const start = runs.find((r) => r.argv.join(" ").indexOf("vmctl start") !== -1)!;
    expect(start.argv).not.toContain("-p");
  });

  it("recovers a VM from vmctl status and derives the guest address", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions },
      run: (argv: string[]) => {
        runs.push({ argv });
        const joined = argv.join(" ");
        if (joined.indexOf("vmctl status") !== -1) {
          return {
            stdout: "   ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME\n" +
              "    7 23456     1    1.0G    200M     ttyp4       root   running wsd-ws-abc\n",
            stderr: "",
            exit_code: 0,
          };
        }
        if (joined.indexOf("test -d") !== -1) {
          return { stdout: "", stderr: "", exit_code: 0 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    const handle = plugin.recover("wsd-ws-abc", "/workspace/repo");
    expect(handle.native_id).toBe("wsd-ws-abc");
    expect(handle.ip).toBe("100.64.7.3");
  });

  it("destroys a VM: stop then remove the disk directory", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions },
      run: (argv: string[]) => {
        runs.push({ argv });
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    plugin.destroyByID("wsd-ws-abc");
    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("doas vmctl stop -f wsd-ws-abc") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("doas rm -rf /var/wsd/vms/openbsd/wsd-ws-abc") !== -1)).toBe(true);
  });

  it("cleanupOrphans removes untracked disk directories only", () => {
    installMockHost({
      options: { ...defaultOptions },
      run: (argv: string[]) => {
        const joined = argv.join(" ");
        if (joined === "ls -1 /var/wsd/vms/openbsd") {
          return { stdout: "wsd-orphan\nwsd-live\nunrelated\ndisk-master.qcow2\n", stderr: "", exit_code: 0 };
        }
        if (joined.indexOf("test -f /var/wsd/vms/openbsd/wsd-orphan/disk.qcow2") !== -1) {
          return { stdout: "", stderr: "", exit_code: 0 };
        }
        if (joined.indexOf("test -f /var/wsd/vms/openbsd/unrelated/disk.qcow2") !== -1) {
          return { stdout: "", stderr: "", exit_code: 1 };
        }
        if (joined.indexOf("test -f /var/wsd/vms/openbsd/disk-master.qcow2/disk.qcow2") !== -1) {
          return { stdout: "", stderr: "", exit_code: 1 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    const removed = plugin.cleanupOrphans(["wsd-live"]);
    expect(removed).toBe(1);
  });
});
