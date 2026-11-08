"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
function networkName() {
  return str(host.options["docker-network"]) || "bridge";
}
const dockerPlugin = {
  name: "docker",
  description: "Docker Engine runtime, driven through the docker CLI",
  version: "1.0.0",
  os: "linux",
  isolation: "container",
  binaries: ["docker", "mkdir"],
  options: [
    {
      flag: "docker-network",
      type: "string",
      default: "bridge",
      usage: "Docker network containers attach to for provisioning and exec network access"
    }
  ],
  provision(spec) {
    const name = "wsd-" + spec.workspace_id;
    const pull = host.run(["docker", "pull", spec.image]);
    if (pull.exit_code !== 0) {
      throw new Error("failed to pull image " + spec.image + ": " + pull.stderr);
    }
    const args = ["docker", "create", "--name", name, "--workdir", spec.work_dir];
    if (spec.cache_mount_path) {
      args.push("-v", spec.cache_mount_path + ":/cache");
    }
    if (spec.workspace_base_path) {
      const hostDir = spec.workspace_base_path + "/" + spec.workspace_id;
      const mk = host.run(["mkdir", "-p", hostDir]);
      if (mk.exit_code !== 0) {
        throw new Error("failed to create workspace dir " + hostDir + ": " + mk.stderr);
      }
      args.push("-v", hostDir + ":" + spec.work_dir);
    }
    args.push(spec.image, "sleep", "infinity");
    const created = host.run(args);
    if (created.exit_code !== 0) {
      throw new Error("failed to create container " + name + ": " + created.stderr);
    }
    const id = str(created.stdout).trim();
    const started = host.run(["docker", "start", id]);
    if (started.exit_code !== 0) {
      host.onFail(["docker", "rm", "-f", id]);
      throw new Error("failed to start container " + name + " (" + id + "): " + started.stderr);
    }
    return { native_id: id, work_dir: spec.work_dir, username: "root", privilege: "root" };
  },
  destroy(handle) {
    host.run(["docker", "rm", "-f", handle.native_id]);
  },
  destroyByID(id) {
    host.run(["docker", "rm", "-f", id]);
  },
  recover(nativeID, workDir) {
    const res = host.run(["docker", "inspect", nativeID]);
    if (res.exit_code !== 0) {
      throw new Error("container " + nativeID + " not found: " + res.stderr);
    }
    return { native_id: nativeID, work_dir: workDir, username: "root", privilege: "root" };
  },
  setNetwork(handle, enabled) {
    const net = networkName();
    if (enabled) {
      const res = host.run(["docker", "network", "connect", net, handle.native_id]);
      if (res.exit_code !== 0 && !/already exists in network/i.test(res.stderr)) {
        throw new Error("docker network connect failed: " + res.stderr);
      }
    } else {
      const res = host.run(["docker", "network", "disconnect", net, handle.native_id]);
      if (res.exit_code !== 0 && !/not connected to network/i.test(res.stderr)) {
        throw new Error("docker network disconnect failed: " + res.stderr);
      }
    }
  },
  execArgs(handle, cmd, workDir, env, opts) {
    const args = ["docker", "exec"];
    if (opts.stdin) args.push("-i");
    if (opts.root) args.push("--user", "0");
    args.push("--workdir", workDir);
    for (const e of env) args.push("--env", e);
    args.push(handle.native_id);
    return args.concat(cmd);
  },
  bootstrapCmd() {
    return ["git", "config", "--system", "--add", "safe.directory", "*"];
  }
};
module.exports = dockerPlugin;
