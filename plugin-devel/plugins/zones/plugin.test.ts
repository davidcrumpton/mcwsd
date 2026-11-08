/**
 * zones/plugin.test.ts
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
  recover: (id: string, workDir: string) => Record<string, unknown>;
  cleanupOrphans: (keep: string[]) => number;
  execArgs: (h: Record<string, unknown>, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[] | { argv: string[]; stdin?: string };
  setNetwork: (h: Record<string, unknown>, enabled: boolean) => void;
  bootstrapCmd: () => string[] | null;
};

interface RunCall {
  argv: string[];
  stdin?: string;
}

const zonesPath = "/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin:/usr/local/bin";

const defaultOptions: Record<string, unknown> = {
  "zones-zonepath": "/rpool1/zones",
  "zones-brand": "ipkg",
  "zones-base-zone": "",
  "zones-network-mode": "shared",
  "zones-interface": "net0",
  "zones-ip-address": "192.168.1.50/24",
  "zones-defrouter": "192.168.1.1",
  "ip-base-network": "",
  "ip-pool-size": 0,
  "zones-dns-servers": "192.168.1.1",
  "zones-escalation": "pfexec",
  "zones-boot-timeout": 1,
  "zones-poll-interval": 0.001,
};

function captureHost(runs: RunCall[], overrides: Record<string, unknown> = {}) {
  return installMockHost({
    options: { ...defaultOptions, ...overrides },
    run: (argv: string[], opts?: { stdin?: string; input?: string }) => {
      runs.push({ argv, stdin: opts?.stdin ?? opts?.input });
      const eff = argv[0] === "pfexec" ? argv.slice(1) : argv;
      if (eff[0] === "zoneadm" && eff[1] === "list" && eff[2] === "-pc") {
        return { stdout: "0:global:running:/::ipkg:shared\n1:wsd-ws-test:running:/x:u:ipkg:shared\n", stderr: "", exit_code: 0 };
      }
      if (eff[0] === "ipadm" && eff[1] === "show-addr") {
        return { stdout: "192.168.1.10/24\n127.0.0.1/8\n", stderr: "", exit_code: 0 };
      }
      if (eff[0] === "zonecfg" && eff.indexOf("info") !== -1) {
        return { stdout: "net:\n", stderr: "", exit_code: 0 };
      }
      if (eff[0] === "dladm" && eff[1] === "show-link") {
        return { stdout: "net0\n", stderr: "", exit_code: 0 };
      }
      if (eff[0] === "dladm" && eff[1] === "show-vnic") {
        return { stdout: "", stderr: "", exit_code: 0 };
      }
      return { stdout: "", stderr: "", exit_code: 0 };
    },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("zones plugin", () => {
  it("declares metadata and the zone tools", () => {
    expect(plugin.name).toBe("zones");
    expect(plugin.os).toBe("illumos");
    expect(plugin.isolation).toBe("zone");
    for (const b of ["zoneadm", "zonecfg", "zlogin", "zfs", "dladm", "ipadm", "pfexec"]) {
      expect(plugin.binaries).toContain(b);
    }
  });

  it("declares the zones-* and reserved ip-* options", () => {
    const flags = plugin.options.map((o) => o.flag);
    for (const want of ["zones-zonepath", "zones-brand", "zones-base-zone", "zones-network-mode", "zones-interface", "ip-base-network", "ip-pool-size", "zones-escalation", "zones-boot-timeout"]) {
      expect(flags).toContain(want);
    }
  });

  it("streams a plain exec through zlogin sh -s on stdin", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = { native_id: "wsd-ws-test", vnic: "" };
    const r = plugin.execArgs(handle, ["git", "status"], "/workspace/repo", ["FOO=bar"], { stdin: false, root: false }) as { argv: string[]; stdin: string };
    expect(r.argv).toEqual(["pfexec", "zlogin", "wsd-ws-test", "sh", "-s"]);
    expect(r.stdin).toContain("cd /workspace/repo &&");
    expect(r.stdin).toContain("env PATH=" + zonesPath);
    expect(r.stdin).toContain("FOO=bar");
    expect(r.stdin).toContain("git status");
  });

  it("maps WriteFile to zlogin tee and CopyDirIn to zlogin tar", () => {
    installMockHost({ options: { ...defaultOptions } });
    const handle = { native_id: "wsd-ws-test", vnic: "" };
    const write = plugin.execArgs(handle, ["sh", "-c", 'cat > "$1"', "sh", "/w/file"], "", [], { stdin: true, root: true }) as string[];
    expect(write).toEqual(["pfexec", "zlogin", "wsd-ws-test", "tee", "/w/file"]);

    const tar = plugin.execArgs(handle, ["tar", "-xf", "-", "-C", "/w"], "", [], { stdin: true, root: true }) as string[];
    expect(tar).toEqual(["pfexec", "zlogin", "wsd-ws-test", "tar", "-xf", "-", "-C", "/w"]);
  });

  it("omits escalation when none is configured", () => {
    installMockHost({ options: { ...defaultOptions, "zones-escalation": "" } });
    const handle = { native_id: "wsd-ws-test", vnic: "" };
    const r = plugin.execArgs(handle, ["true"], "", [], { stdin: false, root: false }) as { argv: string[] };
    expect(r.argv).toEqual(["zlogin", "wsd-ws-test", "sh", "-s"]);
  });

  it("returns the zones bootstrap script", () => {
    installMockHost({ options: { ...defaultOptions } });
    const cmd = plugin.bootstrapCmd()!;
    expect(cmd[0]).toBe("sh");
    expect(cmd[2]).toContain("pkg install git");
    expect(cmd[2]).toContain("safe.directory");
  });

  it("is a no-op for shared-IP networking", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    plugin.setNetwork({ native_id: "wsd-ws-test", vnic: "" }, true);
    plugin.setNetwork({ native_id: "wsd-ws-test", vnic: "" }, false);
    expect(runs.length).toBe(0);
  });

  it("toggles exclusive-IP networking via zlogin ifconfig", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { ...defaultOptions, "zones-network-mode": "exclusive" },
      run: (argv: string[]) => {
        runs.push({ argv });
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    plugin.setNetwork({ native_id: "wsd-ws-test", vnic: "wsdws0" }, false);
    plugin.setNetwork({ native_id: "wsd-ws-test", vnic: "wsdws0" }, true);
    expect(runs[0].argv).toEqual(["pfexec", "zlogin", "wsd-ws-test", "ifconfig", "wsdws0", "down"]);
    expect(runs[1].argv).toEqual(["pfexec", "zlogin", "wsd-ws-test", "ifconfig", "wsdws0", "up"]);
  });

  it("provisions a shared-IP zone and writes the expected zonecfg", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    const handle = plugin.provision({ workspace_id: "ws-test", work_dir: "/workspace/repo", workspace_base_path: "/srv/wsd", cache_mount_path: "/cache" });

    expect(handle.native_id).toBe("wsd-ws-test");
    expect(handle.os).toBe("illumos");

    const tee = runs.find((r) => r.argv[0] === "tee" && r.argv[1].startsWith("/tmp/wsd-zonecfg-"))!;
    expect(tee).toBeTruthy();
    const cfg = tee.stdin ?? "";
    expect(cfg).toContain("create\n");
    expect(cfg).toContain("set brand=ipkg");
    expect(cfg).toContain("set zonepath=/rpool1/zones/wsd-ws-test");
    expect(cfg).toContain("set ip-type=shared");
    expect(cfg).toContain("set physical=net0");
    expect(cfg).toContain("set address=192.168.1.50/24");
    expect(cfg).toContain("set defrouter=192.168.1.1");
    expect(cfg).toContain("set dir=/workspace/repo");
    expect(cfg).toContain("set special=/srv/wsd/ws-test");
    expect(cfg).toContain("set dir=/cache");
    expect(cfg).toContain("commit");

    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("pfexec zoneadm -z wsd-ws-test install") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("pfexec zoneadm -z wsd-ws-test boot") !== -1)).toBe(true);
  });

  it("clones a golden zone and strips its net/fs resources", () => {
    const runs: RunCall[] = [];
    captureHost(runs, { "zones-base-zone": "wsd-golden" });
    plugin.provision({ workspace_id: "ws-test", work_dir: "/workspace/repo" });

    const tee = runs.find((r) => r.argv[0] === "tee" && r.argv[1].startsWith("/tmp/wsd-zonecfg-"))!;
    const cfg = tee.stdin ?? "";
    expect(cfg).toContain("create -t wsd-golden");
    expect(cfg).toContain("remove net");
    expect(cfg).toContain("remove fs");

    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("pfexec zoneadm -z wsd-ws-test clone -X wsd-golden") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("pfexec zoneadm -z wsd-ws-test unmount") !== -1)).toBe(true);
  });

  it("configures exclusive-IP zones with a VNIC and no address", () => {
    const runs: RunCall[] = [];
    captureHost(runs, { "zones-network-mode": "exclusive", "zones-interface": "e1000g0" });
    plugin.provision({ workspace_id: "ws-test", work_dir: "/workspace/repo" });

    const tee = runs.find((r) => r.argv[0] === "tee" && r.argv[1].startsWith("/tmp/wsd-zonecfg-"))!;
    const cfg = tee.stdin ?? "";
    expect(cfg).toContain("set ip-type=exclusive");
    expect(cfg).not.toContain("set address=");

    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("pfexec dladm create-vnic -l e1000g0") !== -1)).toBe(true);
  });

  it("destroys a zone: halt, uninstall, delete, and release the VNIC", () => {
    const runs: RunCall[] = [];
    captureHost(runs, { "zones-network-mode": "exclusive" });
    plugin.destroyByID("wsd-ws-test");
    const joined = runs.map((r) => r.argv.join(" "));
    expect(joined.some((c) => c.indexOf("pfexec zoneadm -z wsd-ws-test halt") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("pfexec zoneadm -z wsd-ws-test uninstall -F") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("pfexec zonecfg -z wsd-ws-test delete -F") !== -1)).toBe(true);
    expect(joined.some((c) => c.indexOf("pfexec dladm delete-vnic") !== -1)).toBe(true);
  });

  it("rejects an invalid network mode", () => {
    installMockHost({ options: { ...defaultOptions, "zones-network-mode": "vnet" } });
    expect(() => plugin.provision({ workspace_id: "ws-test", work_dir: "/w" })).toThrow(/network mode/);
  });
});
