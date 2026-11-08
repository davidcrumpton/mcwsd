/**
 * kubernetes/plugin.test.ts
 */

import { describe, it, expect, beforeEach, vi } from "vitest";
import { installMockHost } from "../mock-host";

import * as pluginModule from "./plugin";
const plugin = pluginModule as unknown as {
  name: string;
  binaries: string[];
  options: { flag: string; type: string; default?: string }[];
  provision: (spec: Record<string, unknown>) => Record<string, unknown>;
  init: () => void;
  recover: (id: string, workDir: string) => Record<string, unknown>;
  execArgs: (h: { native_id: string }, cmd: string[], wd: string, env: string[], o: { stdin: boolean; root: boolean }) => string[];
  setNetwork: (h: { native_id: string }, enabled: boolean) => void;
};

interface RunCall {
  argv: string[];
  stdin?: string;
}

beforeEach(() => {
  vi.clearAllMocks();
});

function captureHost(runs: RunCall[]) {
  return installMockHost({
    options: { "kubernetes-namespace": "default", "kubernetes-kubeconfig": "" },
    run: (argv: string[], opts?: { stdin?: string; input?: string }) => {
      runs.push({ argv, stdin: opts?.stdin ?? opts?.input });
      if (argv.includes("jsonpath={.status.phase}")) {
        return { stdout: "Running\n", stderr: "", exit_code: 0 };
      }
      return { stdout: "", stderr: "", exit_code: 0 };
    },
  });
}

describe("kubernetes plugin", () => {
  it("declares its metadata and namespace options", () => {
    expect(plugin.name).toBe("kubernetes");
    expect(plugin.binaries).toContain("kubectl");
    expect(plugin.options.map((o) => o.flag)).toContain("kubernetes-namespace");
    expect(plugin.options.map((o) => o.flag)).toContain("kubernetes-kubeconfig");
  });

  it("maps a plain exec to a kubectl exec argv with sh -c", () => {
    installMockHost({
      options: { "kubernetes-namespace": "default", "kubernetes-kubeconfig": "" },
    });
    const argv = plugin.execArgs({ native_id: "wsd-ws" }, ["true"], "/w", [], { stdin: false, root: false });
    expect(argv).toEqual(["kubectl", "-n", "default", "exec", "wsd-ws", "--", "sh", "-c", "cd /w && true"]);
  });

  it("adds -i for a stdin file op", () => {
    installMockHost({
      options: { "kubernetes-namespace": "default", "kubernetes-kubeconfig": "" },
    });
    const argv = plugin.execArgs({ native_id: "wsd-ws" }, ["cat"], "", [], { stdin: true, root: true });
    expect(argv).toEqual(["kubectl", "-n", "default", "exec", "-i", "wsd-ws", "--", "sh", "-c", "cat"]);
  });

  it("renders env and quoted args into the remote command", () => {
    installMockHost({
      options: { "kubernetes-namespace": "default", "kubernetes-kubeconfig": "" },
    });
    const argv = plugin.execArgs(
      { native_id: "wsd-ws" },
      ["printf", "%s", "value with spaces; touch /tmp/nope"],
      "/workspace/repo with spaces",
      ["VALUE=two words"],
      { stdin: false, root: false },
    );
    expect(argv).toEqual([
      "kubectl",
      "-n",
      "default",
      "exec",
      "wsd-ws",
      "--",
      "sh",
      "-c",
      "cd '/workspace/repo with spaces' && env 'VALUE=two words' printf %s 'value with spaces; touch /tmp/nope'",
    ]);
  });

  it("omits --kubeconfig when empty and includes it when set", () => {
    installMockHost({
      options: { "kubernetes-namespace": "default", "kubernetes-kubeconfig": "/home/u/.kube/config" },
    });
    const argv = plugin.execArgs({ native_id: "wsd-ws" }, ["true"], "/w", [], { stdin: false, root: false });
    expect(argv[0]).toBe("kubectl");
    expect(argv.slice(0, 5)).toEqual(["kubectl", "--kubeconfig", "/home/u/.kube/config", "-n", "default"]);
  });

  it("applies a pod manifest with the expected shape during provision", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    const handle = plugin.provision({ workspace_id: "ws-123", image: "alpine:latest", work_dir: "/workspace/repo" });
    expect(handle.native_id).toBe("wsd-ws-123");

    const apply = runs.find((r) => r.argv.includes("apply"));
    expect(apply).toBeTruthy();
    const manifest = JSON.parse(apply!.stdin!);
    expect(manifest.kind).toBe("Pod");
    expect(manifest.apiVersion).toBe("v1");
    expect(manifest.metadata.name).toBe("wsd-ws-123");
    expect(manifest.metadata.labels["wsd.io/workspace-key"]).toMatch(/^[0-9a-f]{8}$/);
    expect(manifest.spec.restartPolicy).toBe("Never");
    expect(manifest.spec.containers[0].image).toBe("alpine:latest");
    expect(manifest.spec.containers[0].command).toEqual(["sleep", "infinity"]);
  });

  it("polls pod phase until Running during provision", () => {
    const runs: RunCall[] = [];
    installMockHost({
      options: { "kubernetes-namespace": "default", "kubernetes-kubeconfig": "" },
      run: (argv: string[], opts?: { stdin?: string; input?: string }) => {
        runs.push({ argv, stdin: opts?.stdin ?? opts?.input });
        if (argv.includes("jsonpath={.status.phase}")) {
          return { stdout: "Pending\n", stderr: "", exit_code: 0 };
        }
        return { stdout: "", stderr: "", exit_code: 0 };
      },
    });
    expect(() => plugin.provision({ workspace_id: "ws-123", image: "alpine", work_dir: "/w" })).toThrow(/timed out/);
    const gets = runs.filter((r) => r.argv.includes("jsonpath={.status.phase}"));
    expect(gets.length).toBe(120);
  });

  it("applies a deny-all NetworkPolicy when disabling the network", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    plugin.setNetwork({ native_id: "wsd-ws" }, false);
    const policyApply = runs.find((r) => r.argv.includes("apply"));
    expect(policyApply).toBeTruthy();
    const policy = JSON.parse(policyApply!.stdin!);
    expect(policy.kind).toBe("NetworkPolicy");
    expect(policy.metadata.name).toMatch(/^wsd-net-/);
    expect(policy.spec.policyTypes).toEqual(["Ingress", "Egress"]);
    expect(policy.spec.ingress).toEqual([]);
    expect(policy.spec.egress).toEqual([]);
  });

  it("enables network by deleting the deny-all policy", () => {
    const runs: RunCall[] = [];
    captureHost(runs);
    plugin.setNetwork({ native_id: "wsd-ws" }, true);
    const del = runs.find((r) => r.argv.includes("delete") && r.argv.includes("networkpolicy"));
    expect(del).toBeTruthy();
    expect(del!.argv).toContain("--ignore-not-found");
  });
});
