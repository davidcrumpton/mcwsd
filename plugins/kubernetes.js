"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
function namespace() {
  return str(host.options["kubernetes-namespace"]) || "default";
}
function kubeconfig() {
  return str(host.options["kubernetes-kubeconfig"]);
}
function podName(workspaceID) {
  return "wsd-" + workspaceID;
}
function kubectlArgs(args) {
  const out = ["kubectl"];
  const kc = kubeconfig();
  if (kc) out.push("--kubeconfig", kc);
  out.push("-n", namespace());
  return out.concat(args);
}
function workspaceKey(name) {
  let h = 2166136261;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = h * 16777619 >>> 0;
  }
  return ("00000000" + h.toString(16)).slice(-8);
}
function quoteArg(arg) {
  if (arg === "") {
    return "''";
  }
  if (!/[\s"'\\$&|;<>()*?[\]{}!#~`]/.test(arg)) {
    return arg;
  }
  return "'" + arg.replace(/'/g, "'\\''") + "'";
}
function buildRemoteCommand(cmd, workDir, env) {
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
function buildPodManifest(name, image, workDir, ns) {
  const container = {
    name: "workspace",
    image,
    command: ["sleep", "infinity"]
  };
  if (workDir !== "") {
    container.workingDir = workDir;
  }
  const manifest = {
    apiVersion: "v1",
    kind: "Pod",
    metadata: {
      name,
      namespace: ns,
      labels: { app: "wsd", "wsd.io/workspace-key": workspaceKey(name) }
    },
    spec: {
      restartPolicy: "Never",
      containers: [container]
    }
  };
  return JSON.stringify(manifest);
}
function buildNetworkPolicyManifest(name, pod, ns) {
  const metadata = {
    name,
    labels: { app: "wsd" }
  };
  if (ns !== "") {
    metadata.namespace = ns;
  }
  const manifest = {
    apiVersion: "networking.k8s.io/v1",
    kind: "NetworkPolicy",
    metadata,
    spec: {
      podSelector: {
        matchLabels: { "wsd.io/workspace-key": workspaceKey(pod) }
      },
      policyTypes: ["Ingress", "Egress"],
      ingress: [],
      egress: []
    }
  };
  return JSON.stringify(manifest);
}
function waitForRunning(name) {
  const maxAttempts = 120;
  for (let i = 0; i < maxAttempts; i++) {
    const res = host.run(kubectlArgs(["get", "pod", name, "-o", "jsonpath={.status.phase}"]));
    if (res.exit_code === 0 && str(res.stdout).trim() === "Running") {
      return;
    }
    host.sleep(1e3);
  }
  throw new Error("timed out waiting for pod " + name + " to become Running");
}
const kubernetesPlugin = {
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
      usage: "Kubernetes namespace pods and network policies are created in"
    },
    {
      flag: "kubernetes-kubeconfig",
      type: "string",
      default: "",
      usage: "Path to a kubeconfig file; empty uses kubectl's own KUBECONFIG resolution"
    }
  ],
  init() {
    const ns = namespace();
    const manifest = JSON.stringify({
      apiVersion: "v1",
      kind: "Namespace",
      metadata: { name: ns }
    });
    const res = host.run(kubectlArgs(["apply", "-f", "-"]), { stdin: manifest });
    if (res.exit_code !== 0) {
      throw new Error("failed to initialize namespace " + ns + ": " + res.stderr);
    }
  },
  provision(spec) {
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
    host.onFail(kubectlArgs(["delete", "pod", name, "--grace-period=5", "--ignore-not-found"]));
    waitForRunning(name);
    return { native_id: name, work_dir: spec.work_dir, username: "root", privilege: "root" };
  },
  destroy(handle) {
    host.run(kubectlArgs(["delete", "pod", handle.native_id, "--grace-period=5", "--ignore-not-found"]));
  },
  destroyByID(id) {
    host.run(kubectlArgs(["delete", "pod", id, "--grace-period=5", "--ignore-not-found"]));
  },
  recover(nativeID, workDir) {
    const res = host.run(kubectlArgs(["get", "pod", nativeID, "-o", "name"]));
    if (res.exit_code !== 0) {
      throw new Error("pod " + nativeID + " not found: " + res.stderr);
    }
    return { native_id: nativeID, work_dir: workDir, username: "root", privilege: "root" };
  },
  setNetwork(handle, enabled) {
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
  execArgs(handle, cmd, workDir, env, opts) {
    const args = kubectlArgs(["exec"]);
    if (opts.stdin) args.push("-i");
    args.push(handle.native_id, "--", "sh", "-c", buildRemoteCommand(cmd, workDir, env));
    return args;
  }
};
module.exports = kubernetesPlugin;
