/**
 * freebsd-jail/plugin.test.ts
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
  setNetwork: (h: Record<string, unknown>, enabled: boolean) => void;
  bootstrapCmd: () => string[] | null;
};

interface RunCall {
  argv: string[];
  stdin?: string;
}

const jailPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin";

const defaultOptions: Record<string, unknown> = {
  "freebsd-release": "15.1-RELEASE",
  "freebsd-arch": "amd64",
  "freebsd-interface": "vtnet0",
  "freebsd-ip4-address": "192.168.122.100",
  "freebsd-network-mode": "shared",
  "freebsd-vnet-gateway": "",
  "ip-base-network": "",
  "ip-pool-size": 0,
  "freebsd-dns-servers": "",
  "escalation-command": "mdo",
  "base-template-url": "",
  "base-template-user": "",
  "base-template-password-env": "",
  "base-template-header": "",
  "base-template-header-env": "",
  "verify-signature": false,
  "template-public-key": "",
};

function captureHost(runs: RunCall[]) {
  return installMockHost({
    options: { ...defaultOptions },
    run: (argv: string[], opts?: { stdin?: string; input?: string }) => {
      runs.push({ argv, stdin: opts?.stdin ?? opts?.input });
      return { stdout: "", stderr: "", exit_code: 0 };
    },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("freebsd-jail plugin", () => {
  it("declares metadata and the jail/jexec/mdo binaries", () => {
    expect(plugin.name).toBe("freebsd-jail");
    expect(plugin.os).toBe("freebsd");
    expect(plugin.isolation).toBe("jail");
    for (const b of ["jail", "jexec", "jls", "tar", "chroot", "mdo", "tee", "fetch"]) {
      expect(plugin.binaries).toContain(b);
    }
  });

  it("declares the freebsd-* and reserved ip-* options", () => {
    const flags = plugin.options.map((o) => o.flag);
    for (const want of ["freebsd-release", "freebsd-interface", "freebsd-network-mode", "ip-base-network", "ip-pool-size", "escalation-command", "base-template-url", "verify-signature"]) {
      expect(flags).toContain(want);
    }
  });

  it("maps an exec to a jexec argv with PATH and escalation", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = { native_id: "wsd-ws-abc", vnet_interface: "" };
    const argv = plugin.execArgs(handle, ["echo", "hi"], "/w", ["A=1"], { stdin: false, root: false });
    expect(argv).toEqual(["mdo", "jexec", "-d", "/w", "wsd-ws-abc", "env", "PATH=" + jailPath, "A=1", "echo", "hi"]);
  });

  it("omits escalation when no escalator is configured", () => {
    installMockHost({ options: { ...defaultOptions, "escalation-command": "" } });
    const handle = { native_id: "wsd-ws-abc" };
    const argv = plugin.execArgs(handle, ["true"], "", [], { stdin: false, root: false });
    expect(argv).toEqual(["jexec", "wsd-ws-abc", "env", "PATH=" + jailPath, "true"]);
  });

  it("returns the jail bootstrap script", () => {
    installMockHost({ options: { ...defaultOptions } });
    const cmd = plugin.bootstrapCmd()!;
    expect(cmd[0]).toBe("sh");
    expect(cmd[1]).toBe("-c");
    const script = cmd[2];
    expect(script.indexOf("certctl rehash")).toBeLessThan(script.indexOf("pkg bootstrap"));
    expect(script).toContain("pkg install -y git");
    expect(script).toContain("safe.directory");
  });

  it("toggles shared-stack networking via jail -m name=", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    plugin.setNetwork({ native_id: "wsd-ws-abc", vnet_interface: "" }, true);
    plugin.setNetwork({ native_id: "wsd-ws-abc", vnet_interface: "" }, false);
    expect(runs[0].argv).toEqual(["mdo", "jail", "-m", "name=wsd-ws-abc", "ip4=inherit"]);
    expect(runs[1].argv).toEqual(["mdo", "jail", "-m", "name=wsd-ws-abc", "ip4=disable"]);
  });

  it("toggles VNET networking via jexec ifconfig", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions, "freebsd-network-mode": "vnet" },
      run: (argv: string[]) => {
        runs.push({ argv });
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    plugin.setNetwork({ native_id: "wsd-ws-abc", vnet_interface: "e0b_wswsabc" }, true);
    expect(runs[0].argv).toEqual(["mdo", "jexec", "wsd-ws-abc", "ifconfig", "e0b_wswsabc", "up"]);
  });

  it("generates a shared-stack jail.conf with the expected mounts", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    const handle = plugin.provision({
      workspace_id: "ws-abc",
      work_dir: "/workspace/repo",
      cache_mount_path: "/host/cache",
      workspace_base_path: "/srv/wsd",
    });

    expect(handle.native_id).toBe("wsd-ws-abc");
    expect(handle.work_dir).toBe("/workspace/repo");
    expect(handle.os).toBe("freebsd");

    const tee = runs.find((r) => r.argv[0] === "tee" && r.argv[1].endsWith(".conf"))!;
    expect(tee).toBeTruthy();
    const config = tee.stdin ?? "";
    expect(config).toContain('interface = "vtnet0";');
    expect(config).toContain('ip4.addr += "192.168.122.100";');
    expect(config).toContain("mount.devfs;");
    expect(config).toContain('/host/cache /usr/local/jails/containers/wsd-ws-abc/cache nullfs rw 0 0');
    expect(config).toContain('/srv/wsd/ws-abc /usr/local/jails/containers/wsd-ws-abc/workspace/repo nullfs rw 0 0');
    expect(config).toContain("allow.sysvipc;");
    expect(config).not.toContain("allow.sysvshm");

    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("mdo jail -c -f") !== -1)).toBe(true);
  });

  it("uses DHCP (ip4 = inherit) when no address is configured", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions, "freebsd-ip4-address": "" },
      run: (argv: string[], opts?: { stdin?: string }) => {
        runs.push({ argv, stdin: opts?.stdin });
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    plugin.provision({ workspace_id: "dhcp", work_dir: "/w" });
    const tee = runs.find((r) => r.argv[0] === "tee" && r.argv[1].endsWith(".conf"))!;
    expect(tee.stdin).toContain("ip4 = inherit;");
    expect(tee.stdin).not.toContain("ip4.addr");
  });

  it("auto-assigns an IP from the pool for VNET", () => {
    const runs: RunCall[] = [];
    const ipam = {
      allocate: vi.fn(() => "192.168.1.200/24"),
      release: vi.fn(),
      reserve: vi.fn(),
      gateway: vi.fn(() => "192.168.1.1"),
      network: vi.fn(() => "192.168.1.0/24"),
    };
    installMockHost({
      options: {
        ...defaultOptions,
        "freebsd-network-mode": "vnet",
        "freebsd-ip4-address": "",
        "ip-base-network": "192.168.1.200/24",
        "ip-pool-size": 50,
        "freebsd-dns-servers": "192.168.1.1",
      },
      run: (argv: string[], opts?: { stdin?: string }) => {
        runs.push({ argv, stdin: opts?.stdin });
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    // Override the mock host's ipam with our capturing double.
    (globalThis as unknown as { host: { ipam: unknown } }).host.ipam = ipam;

    plugin.provision({ workspace_id: "ws-abc", work_dir: "/w" });

    expect(ipam.allocate).toHaveBeenCalledWith("ws-abc");
    expect(ipam.gateway).toHaveBeenCalled();
    const tee = runs.find((r) => r.argv[0] === "tee" && r.argv[1].endsWith(".conf"))!;
    expect(tee.stdin).toContain("/sbin/ifconfig e0b_wsabc 192.168.1.200/24 up");
    expect(tee.stdin).toContain("/sbin/route add default 192.168.1.1");
  });

  it("destroys a jail: stop, remove config, unmount, remove root, release IP", () => {
    const runs: RunCall[] = [];
    const released: string[] = [];
    installMockHost({
      options: { ...defaultOptions },
      run: (argv: string[]) => {
        runs.push({ argv });
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    (globalThis as unknown as { host: { ipam: { release: (id: string) => void } } }).host.ipam.release = (id) => released.push(id);

    plugin.destroyByID("wsd-ws-abc");
    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("mdo jail -r wsd-ws-abc") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("rm -f /etc/jail.conf.d/wsd-ws-abc.conf") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("mdo rm -rf /usr/local/jails/containers/wsd-ws-abc") !== -1)).toBe(true);
    expect(released).toContain("ws-abc");
  });

  it("recovers a jail and reserves its recorded IP", () => {
    const reserved: Array<[string, string]> = [];
    installMockHost({
      options: { ...defaultOptions, "ip-base-network": "192.168.1.200/24", "ip-pool-size": 8 },
      run: (argv: string[]) => {
        if (argv[0] === "cat") {
          return { stdout: 'ip4.addr += "192.168.1.201/24";\n', stderr: "", exit_code: 0 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    (globalThis as unknown as { host: { ipam: { reserve: (id: string, addr: string) => void } } }).host.ipam.reserve = (id, addr) => reserved.push([id, addr]);

    const handle = plugin.recover("wsd-ws-abc", "/workspace/repo");
    expect(handle.native_id).toBe("wsd-ws-abc");
    expect(reserved).toContainEqual(["ws-abc", "192.168.1.201"]);
  });

  it("rejects an invalid release", () => {
    installMockHost({ options: { ...defaultOptions, "freebsd-release": "../etc" } });
    expect(() => plugin.provision({ workspace_id: "bad", work_dir: "/w" })).toThrow(/release/);
  });
});
