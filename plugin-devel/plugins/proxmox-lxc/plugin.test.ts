/**
 * proxmox-lxc/plugin.test.ts
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
  bootstrapCmd: () => string[] | null;
};

interface RunCall {
  argv: string[];
}

const node = "pve1";

const defaultOptions: Record<string, unknown> = {
  "proxmox-api-url": "https://pve.example:8006",
  "proxmox-node": node,
  "proxmox-token-env": "PVE_API_TOKEN",
  "proxmox-insecure-tls": true,
  "proxmox-storage": "local",
  "proxmox-ostemplate": "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
  "proxmox-ostype": "debian",
  "proxmox-rootfs-size": 8,
  "proxmox-bridge": "vmbr0",
  "proxmox-ip-address": "",
  "proxmox-gateway": "",
  "proxmox-dns-servers": "",
  "ip-base-network": "",
  "ip-pool-size": 0,
  "proxmox-memory": 512,
  "proxmox-swap": 512,
  "proxmox-cores": 1,
  "proxmox-ssh-key": "/var/wsd/agent_ed25519",
  "proxmox-ssh-public-key": "/var/wsd/agent_ed25519.pub",
  "proxmox-ssh-port": 22,
  "proxmox-login-user": "root",
  "proxmox-guest-escalation-command": "sudo",
  "proxmox-boot-timeout": 300,
  "proxmox-poll-interval": 2,
  "proxmox-ssh-probe-timeout": 20,
};

// vmID mirrors the plugin's deterministic workspaceVMID so tests can assert
// the exact VMID without duplicating the plugin's hash through a string.
function vmID(workspaceID: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < workspaceID.length; i++) {
    h ^= workspaceID.charCodeAt(i);
    h = (h * 0x01000193) >>> 0;
  }
  return String(100 + (h % 999999900));
}

const vmid = vmID("ws-abc");

function url(argv: string[]): string {
  return argv[argv.length - 1];
}

function curlMethod(argv: string[]): string {
  const i = argv.indexOf("-X");
  return i !== -1 ? argv[i + 1] : "GET";
}

function apiResponse(argv: string[]): { stdout: string; stderr: string; exit_code: number } {
  const path = url(argv);
  const method = curlMethod(argv);
  const ok = (body: string) => ({ stdout: body, stderr: "", exit_code: 0 });
  if (path.indexOf("/tasks/") !== -1) return ok('{"data":{"status":"stopped","exitstatus":"OK"}}');
  if (path.indexOf("/lxc/" + vmid + "/status/current") !== -1) return ok('{"data":{"status":"running"}}');
  if (path.indexOf("/lxc/" + vmid + "/interfaces") !== -1) return ok('{"data":[{"name":"eth0","inet":"192.168.1.99"}]}');
  if (path.indexOf("/lxc/" + vmid + "/status/stop") !== -1) return ok('{"data":null}');
  if (path.indexOf("/lxc/" + vmid + "/status/start") !== -1) return ok('{"data":"UPID:pve1:1:1:1:start:' + vmid + ':root@pam:"}');
  if (path.indexOf("/lxc/" + vmid) !== -1) return ok('{"data":null}'); // DELETE
  if (path.indexOf("/nodes/pve1/lxc") !== -1) {
    if (method === "POST") return ok('{"data":"UPID:pve1:00002F9D:000DC5EA:start:' + vmid + ':root@pam:"}');
    return ok('{"data":[{"vmid":' + vmid + ',"name":"wsd-ws-abc","status":"running","type":"lxc"}]}');
  }
  if (path.indexOf("/nodes/pve1/status") !== -1) return ok('{"data":{}}');
  return ok("{}");
}

function captureHost(runs: RunCall[], responses?: (argv: string[]) => { stdout: string; stderr: string; exit_code: number }) {
  return installMockHost({
    options: { ...defaultOptions },
    run: (argv: string[]) => {
      runs.push({ argv });
      if (argv[0] === "curl") {
        return (responses ?? apiResponse)(argv);
      }
      if (argv[0] === "cat") {
        return { stdout: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test key\n", stderr: "", exit_code: 0 };
      }
      if (argv[0] === "nc") {
        return { stdout: "", stderr: "", exit_code: 0 };
      }
      if (argv[0] === "ssh") {
        return { stdout: "", stderr: "", exit_code: 0 };
      }
      if (argv[0] === "test") {
        return { stdout: "", stderr: "", exit_code: 0 };
      }
      return { stdout: "", stderr: "", exit_code: 0 };
    },
  });
}

function formOf(run: RunCall): Record<string, string> {
  const form: Record<string, string> = {};
  for (let i = 0; i < run.argv.length - 1; i++) {
    if (run.argv[i] === "--data-urlencode") {
      const kv = run.argv[i + 1];
      const eq = kv.indexOf("=");
      form[kv.slice(0, eq)] = kv.slice(eq + 1);
    }
  }
  return form;
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("proxmox-lxc plugin", () => {
  it("declares metadata and the curl/ssh binaries", () => {
    expect(plugin.name).toBe("proxmox-lxc");
    expect(plugin.os).toBe("linux");
    expect(plugin.isolation).toBe("container");
    for (const b of ["curl", "ssh", "nc", "cat", "test"]) {
      expect(plugin.binaries).toContain(b);
    }
  });

  it("declares the proxmox-* and reserved ip-* options", () => {
    const flags = plugin.options.map((o) => o.flag);
    for (const want of ["proxmox-api-url", "proxmox-node", "proxmox-token-env", "proxmox-ostemplate", "proxmox-ip-address", "ip-base-network", "ip-pool-size", "proxmox-ssh-key", "proxmox-boot-timeout"]) {
      expect(flags).toContain(want);
    }
  });

  it("maps an exec to an ssh argv with a PATH and cd payload", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = {
      native_id: "wsd-ws-abc",
      ip: "192.168.1.50",
      port: 22,
      username: "root",
      key_path: "/var/wsd/agent_ed25519",
      escalation: "",
    };
    const argv = plugin.execArgs(handle, ["echo", "hi"], "/workspace/repo", [], { stdin: false, root: false });
    expect(argv[0]).toBe("ssh");
    expect(argv).toContain("-i");
    expect(argv).toContain("/var/wsd/agent_ed25519");
    expect(argv).toContain("root@192.168.1.50");
    const payload = argv[argv.length - 1];
    expect(payload).toContain("cd /workspace/repo && PATH=");
    expect(payload).toContain("echo hi");
  });

  it("wraps the payload in sudo sh -c for a non-root login user", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = {
      native_id: "wsd-ws-abc",
      ip: "192.168.1.50",
      port: 22,
      username: "wsd",
      key_path: "/var/wsd/agent_ed25519",
      escalation: "sudo",
    };
    const argv = plugin.execArgs(handle, ["echo", "hi"], "/w", [], { stdin: false, root: false });
    expect(argv[argv.length - 1]).toContain("sudo sh -c");
  });

  it("returns the apt bootstrap script", () => {
    installMockHost({ options: { ...defaultOptions } });
    const cmd = plugin.bootstrapCmd()!;
    expect(cmd[0]).toBe("sh");
    expect(cmd[2]).toContain("apt-get install -y");
    expect(cmd[2]).toContain("safe.directory");
  });

  it("provisions a DHCP container: create, wait, read the IP, then SSH", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    const handle = plugin.provision({ workspace_id: "ws-abc", work_dir: "/workspace/repo" });

    expect(handle.native_id).toBe("wsd-ws-abc");
    expect(handle.ip).toBe("192.168.1.99");
    expect(handle.os).toBe("linux");

    const create = runs.find((r) => r.argv[0] === "curl" && url(r.argv).indexOf("/nodes/pve1/lxc") !== -1)!;
    expect(create).toBeTruthy();
    const form = formOf(create);
    expect(form.vmid).toBe(vmid);
    expect(form.hostname).toBe("wsd-ws-abc");
    expect(form.net0).toBe("name=eth0,bridge=vmbr0,ip=dhcp");
    expect(form["ssh-public-keys"]).toContain("ssh-ed25519");
    expect(form.unprivileged).toBe("1");

    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("/interfaces") !== -1)).toBe(true);
    expect(runs.some((r) => r.argv[0] === "ssh")).toBe(true);
  });

  it("uses a static address from the option when set", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions, "proxmox-ip-address": "192.168.1.50/24", "proxmox-gateway": "192.168.1.1" },
      run: (argv: string[]) => {
        runs.push({ argv });
        if (argv[0] === "curl") return apiResponse(argv);
        if (argv[0] === "cat") return { stdout: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test key\n", stderr: "", exit_code: 0 };
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    const handle = plugin.provision({ workspace_id: "ws-abc", work_dir: "/w" });
    expect(handle.ip).toBe("192.168.1.50");

    const create = runs.find((r) => r.argv[0] === "curl" && url(r.argv).indexOf("/nodes/pve1/lxc") !== -1)!;
    expect(formOf(create).net0).toBe("name=eth0,bridge=vmbr0,ip=192.168.1.50/24,gw=192.168.1.1");
    // Static address is known up front, so the interfaces endpoint is not polled.
    expect(runs.some((r) => r.argv.join(" ").indexOf("/interfaces") !== -1)).toBe(false);
  });

  it("auto-assigns an IP from the shared pool and uses the pool gateway", () => {
    const runs: RunCall[] = [];
    const ipam = {
      allocate: vi.fn(() => "192.168.1.200/24"),
      release: vi.fn(),
      reserve: vi.fn(),
      gateway: vi.fn(() => "192.168.1.1"),
      network: vi.fn(() => "192.168.1.0/24"),
    };
    installMockHost({
      options: { ...defaultOptions, "ip-base-network": "192.168.1.200/24", "ip-pool-size": 50 },
      run: (argv: string[]) => {
        runs.push({ argv });
        if (argv[0] === "curl") return apiResponse(argv);
        if (argv[0] === "cat") return { stdout: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test key\n", stderr: "", exit_code: 0 };
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    (globalThis as unknown as { host: { ipam: unknown } }).host.ipam = ipam;

    plugin.provision({ workspace_id: "ws-abc", work_dir: "/w" });

    expect(ipam.allocate).toHaveBeenCalledWith("ws-abc");
    expect(ipam.gateway).toHaveBeenCalled();
    const create = runs.find((r) => r.argv[0] === "curl" && url(r.argv).indexOf("/nodes/pve1/lxc") !== -1)!;
    expect(formOf(create).net0).toBe("name=eth0,bridge=vmbr0,ip=192.168.1.200/24,gw=192.168.1.1");
  });

  it("destroys a container: stop, delete, and release the pooled IP", () => {
    const runs: RunCall[] = [];
    const released: string[] = [];
    captureHost(runs);
    (globalThis as unknown as { host: { ipam: { release: (id: string) => void } } }).host.ipam.release = (id) => released.push(id);

    plugin.destroyByID("wsd-ws-abc");
    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("/status/stop") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("DELETE") !== -1 && c.indexOf("/lxc/" + vmid) !== -1)).toBe(true);
    expect(released).toContain("ws-abc");
  });

  it("recovers a container, reserving its reported IP", () => {
    const reserved: Array<[string, string]> = [];
    installMockHost({
      options: { ...defaultOptions, "ip-base-network": "192.168.1.200/24", "ip-pool-size": 8 },
      run: (argv: string[]) => {
        if (argv[0] === "curl") return apiResponse(argv);
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    (globalThis as unknown as { host: { ipam: { reserve: (id: string, addr: string) => void } } }).host.ipam.reserve = (id, addr) => reserved.push([id, addr]);

    const handle = plugin.recover("wsd-ws-abc", "/workspace/repo");
    expect(handle.native_id).toBe("wsd-ws-abc");
    expect(handle.ip).toBe("192.168.1.99");
    expect(reserved).toContainEqual(["ws-abc", "192.168.1.99"]);
  });

  it("cleanupOrphans deletes untracked wsd- containers only", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions },
      run: (argv: string[]) => {
        runs.push({ argv });
        if (argv[0] === "curl") {
          const u = url(argv);
          if (u.indexOf("/nodes/pve1/lxc") !== -1) {
            return {
              stdout: '{"data":[{"vmid":600,"name":"wsd-live","status":"running"},{"vmid":601,"name":"wsd-orphan","status":"running"},{"vmid":602,"name":"unrelated","status":"running"}]}',
              stderr: "",
              exit_code: 0,
            };
          }
          return { stdout: "{}", stderr: "", exit_code: 0 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    const removed = plugin.cleanupOrphans(["wsd-live"]);
    expect(removed).toBe(1);
    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("/lxc/601") !== -1 && c.indexOf("DELETE") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("/lxc/600") !== -1 && c.indexOf("DELETE") !== -1)).toBe(false);
  });

  it("rejects an invalid ip-address", () => {
    installMockHost({ options: { ...defaultOptions, "proxmox-ip-address": "not-a-cidr" } });
    expect(() => plugin.provision({ workspace_id: "bad", work_dir: "/w" })).toThrow(/CIDR|invalid/);
  });
});
