"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
const podmanPlugin = {
  name: "podman",
  description: "Podman rootless OCI runtime, driven through the podman CLI",
  version: "1.0.0",
  os: "linux",
  isolation: "container",
  binaries: ["podman", "mkdir"],
  options: [],
  provision(spec) {
    const name = "wsd-" + spec.workspace_id;
    const pull = host.run(["podman", "pull", spec.image]);
    if (pull.exit_code !== 0) {
      throw new Error("failed to pull image " + spec.image + ": " + pull.stderr);
    }
    const args = ["podman", "create", "--name", name, "--workdir", spec.work_dir];
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
    const started = host.run(["podman", "start", id]);
    if (started.exit_code !== 0) {
      host.onFail(["podman", "rm", "-f", id]);
      throw new Error("failed to start container " + name + " (" + id + "): " + started.stderr);
    }
    return { native_id: id, work_dir: spec.work_dir, username: "root", privilege: "root" };
  },
  destroy(handle) {
    host.run(["podman", "rm", "-f", handle.native_id]);
  },
  destroyByID(id) {
    host.run(["podman", "rm", "-f", id]);
  },
  execArgs(handle, cmd, workDir, env, opts) {
    const args = ["podman", "exec"];
    if (opts.stdin) args.push("-i");
    if (opts.root) args.push("--user", "0");
    args.push("--workdir", workDir);
    for (const e of env) args.push("--env", e);
    args.push(handle.native_id);
    return args.concat(cmd);
  }
};
module.exports = podmanPlugin;
