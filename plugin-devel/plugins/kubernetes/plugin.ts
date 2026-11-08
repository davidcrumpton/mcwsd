/// <reference path="../../types/mcwsd-runtime.d.ts" />
import type { RuntimePlugin, ProvisionSpec, Handle, ExecArgsOptions } from "../../types/mcwsd-runtime";
"use strict";

/**
 * Kubernetes runtime plugin: drives one Pod per workspace entirely through the
 * kubectl CLI, matching the other runtimes (which all shell out rather than
 * link a client library). This replaces the former native kubectl backend.
 *
 * Options:
 *   --kubernetes-namespace   namespace pods and network policies live in.
 *   --kubernetes-kubeconfig  path to a kubeconfig; empty means kubectl's own
 *                            resolution (KUBECONFIG / ~/.kube/config).
 *
 * Like the native backend, exec has no `--workdir`/`--env` equivalent on the
 * kubectl CLI, so the command is rendered into a single shell string
 * (cd + env + quoted argv) and run as `kubectl exec <pod> -- sh -c '<payload>'`.
 * That shell quoting is the hard-won quirk this port keeps; core still owns
 * ReadFile/WriteFile/CopyDirIn on top of execArgs.
 */

function str(v: unknown): string {
  return typeof v === "string" ? v : v === undefined || v === null ? "" : String(v);
}

function namespace(): string {
  return str(host.options["kubernetes-namespace"]) || "default";
}

function kubeconfig(): string {
  return str(host.options["kubernetes-kubeconfig"]);
}

function podName(workspaceID: string): string {
  return "wsd-" + workspaceID;
}

// kubectlArgs prepends the shared kubeconfig/-n flags. The kubeconfig flag is
// omitted when empty so kubectl falls back to its own KUBECONFIG resolution.
function kubectlArgs(args: string[]): string[] {
  const out = ["kubectl"];
  const kc = kubeconfig();
  if (kc) out.push("--kubeconfig", kc);
  out.push("-n", namespace());
  return out.concat(args);
}

// workspaceKey maps a pod name to a fixed-width hex string valid as a
// Kubernetes label value. The native backend used a SHA-256 hex prefix; goja
// has no crypto, so this port uses a FNV-1a 32-bit hash. It only needs
// stability and uniqueness for the pod <-> NetworkPolicy label selector, not
// cryptographic strength, and pod names on one daemon are unique.
function workspaceKey(name: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = (h * 0x01000193) >>> 0;
  }
  return ("00000000" + h.toString(16)).slice(-8);
}

function quoteArg(arg: string): string {
  if (arg === "") {
    return "''";
  }
  if (!/[\s"'\\$&|;<>()*?[\]{}!#~`]/.test(arg)) {
    return arg;
  }
  return "'" + arg.replace(/'/g, "'\\''") + "'";
}

// buildRemoteCommand renders env assignments, then cd, then the command, all
// interpreted by the container's shell rather than the local one. Mirrors the
// native transport's helper of the same name.
function buildRemoteCommand(cmd: string[], workDir: string, env: string[]): string {
  const parts = cmd.map((arg) => quoteArg(arg));
  let cmdStr = parts.join(" ");
  if (cmdStr === "") {
    cmdStr = ":";
  }
  if (env.length > 0) {
    const assignments = env.map((assignment) => quoteArg(assignment));
    cmdStr = "env " + assignments.join(" ") + " " + cmdStr;
  }
  if (workDir !== "") {
    cmdStr = "cd " + quoteArg(workDir) + " && " + cmdStr;
  }
  return cmdStr;
}

function buildPodManifest(name: string, image: string, workDir: string, ns: string): string {
  const container: Record<string, unknown> = {
    name: "workspace",
    image: image,
    command: ["sleep", "infinity"],
  };
  if (workDir !== "") {
    container.workingDir = workDir;
  }
  const manifest = {
    apiVersion: "v1",
    kind: "Pod",
    metadata: {
      name: name,
      namespace: ns,
      labels: { app: "wsd", "wsd.io/workspace-key": workspaceKey(name) },
    },
    spec: {
      restartPolicy: "Never",
      containers: [container],
    },
  };
  return JSON.stringify(manifest);
}

function buildNetworkPolicyManifest(name: string, pod: string, ns: string): string {
  const metadata: Record<string, unknown> = {
    name: name,
    labels: { app: "wsd" },
  };
  if (ns !== "") {
    metadata.namespace = ns;
  }
  const manifest = {
    apiVersion: "networking.k8s.io/v1",
    kind: "NetworkPolicy",
    metadata: metadata,
    spec: {
      podSelector: {
        matchLabels: { "wsd.io/workspace-key": workspaceKey(pod) },
      },
      policyTypes: ["Ingress", "Egress"],
      ingress: [],
      egress: [],
    },
  };
  return JSON.stringify(manifest);
}

// waitForRunning polls pod phase the same way the native provisioner did:
// there is no blocking "create and wait" verb in kubectl itself. host.sleep
// honors request cancellation, so a cancelled provision aborts the loop.
function waitForRunning(name: string): void {
  const maxAttempts = 120; // 2 minutes at 1s
  for (let i = 0; i < maxAttempts; i++) {
    const res = host.run(kubectlArgs(["get", "pod", name, "-o", "jsonpath={.status.phase}"]));
    if (res.exit_code === 0 && str(res.stdout).trim() === "Running") {
      return;
    }
    host.sleep(1000);
  }
  throw new Error("timed out waiting for pod " + name + " to become Running");
}

const kubernetesPlugin: RuntimePlugin = {
  name: "kubernetes",
  description: "Kubernetes pods, driven through the kubectl CLI",
  version: "1.0.0",
  os: "linux",
  isolation: "container",
  binaries: ["kubectl"],
  options: [
    {
      flag: "kubernetes-namespace",
      type: "string",
      default: "default",
      usage: "Kubernetes namespace pods and network policies are created in",
    },
    {
      flag: "kubernetes-kubeconfig",
      type: "string",
      default: "",
      usage: "Path to a kubeconfig file; empty uses kubectl's own KUBECONFIG resolution",
    },
  ],

  init(): void {
    const ns = namespace();
    const manifest = JSON.stringify({
      apiVersion: "v1",
      kind: "Namespace",
      metadata: { name: ns },
    });
    const res = host.run(kubectlArgs(["apply", "-f", "-"]), { stdin: manifest });
    if (res.exit_code !== 0) {
      throw new Error("failed to initialize namespace " + ns + ": " + res.stderr);
    }
  },

  provision(spec: ProvisionSpec): Handle {
    if (!spec.workspace_id) {
      throw new Error("workspace ID must not be empty");
    }
    if (!spec.image) {
      throw new Error("image must be set in the provision spec or provisioner defaults");
    }

    const name = podName(spec.workspace_id);
    const manifest = buildPodManifest(name, spec.image, spec.work_dir, namespace());

    const applied = host.run(kubectlArgs(["apply", "-f", "-"]), { stdin: manifest });
    if (applied.exit_code !== 0) {
      throw new Error("failed to create pod " + name + ": " + applied.stderr);
    }
    // Undo the apply if anything downstream (the Running wait) fails.
    host.onFail(kubectlArgs(["delete", "pod", name, "--grace-period=5", "--ignore-not-found"]));

    waitForRunning(name);

    return { native_id: name, work_dir: spec.work_dir, username: "root", privilege: "root" };
  },

  destroy(handle: Handle): void {
    host.run(kubectlArgs(["delete", "pod", handle.native_id, "--grace-period=5", "--ignore-not-found"]));
  },

  destroyByID(id: string): void {
    host.run(kubectlArgs(["delete", "pod", id, "--grace-period=5", "--ignore-not-found"]));
  },

  recover(nativeID: string, workDir: string): Handle {
    const res = host.run(kubectlArgs(["get", "pod", nativeID, "-o", "name"]));
    if (res.exit_code !== 0) {
      throw new Error("pod " + nativeID + " not found: " + res.stderr);
    }
    return { native_id: nativeID, work_dir: workDir, username: "root", privilege: "root" };
  },

  setNetwork(handle: Handle, enabled: boolean): void {
    const policyName = "wsd-net-" + workspaceKey(handle.native_id);
    if (enabled) {
      const res = host.run(kubectlArgs(["delete", "networkpolicy", policyName, "--ignore-not-found"]));
      if (res.exit_code !== 0) {
        throw new Error("failed to enable network for pod " + handle.native_id + ": " + res.stderr);
      }
    } else {
      const manifest = buildNetworkPolicyManifest(policyName, handle.native_id, namespace());
      const res = host.run(kubectlArgs(["apply", "-f", "-"]), { stdin: manifest });
      if (res.exit_code !== 0) {
        throw new Error("failed to disable network for pod " + handle.native_id + ": " + res.stderr);
      }
    }
  },

  execArgs(handle: Handle, cmd: string[], workDir: string, env: string[], opts: ExecArgsOptions): string[] {
    const args = kubectlArgs(["exec"]);
    if (opts.stdin) args.push("-i");
    args.push(handle.native_id, "--", "sh", "-c", buildRemoteCommand(cmd, workDir, env));
    return args;
  },
};

module.exports = kubernetesPlugin;
