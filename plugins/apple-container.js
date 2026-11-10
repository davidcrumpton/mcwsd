"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
function networkName() {
  return str(host.options["container-network"]) || "default";
}
function containerName(workspaceID) {
  return "wsd-" + workspaceID;
}
const appleContainerPlugin = {
  name: "apple-container",
  description: "Apple OCI containers on macOS, driven through the container CLI",
  version: "1.0.0",
  os: "linux",
  isolation: "container",
  binaries: ["container", "mkdir"],
  options: [
    {
      flag: "container-network",
      type: "string",
      default: "default",
      usage: 'Apple container network attached at create time (use "none" for no network; attachment cannot be changed later)'
    }
  ],
  provision(spec) {
    const name = containerName(spec.workspace_id);
    const pull = host.run(["container", "image", "pull", spec.image]);
    if (pull.exit_code !== 0) {
      throw new Error("failed to pull image " + spec.image + ": " + pull.stderr);
    }
    const args = [
      "container",
      "create",
      "--name",
      name,
      "--workdir",
      spec.work_dir,
      "--network",
      networkName()
    ];
    if (spec.cache_mount_path) {
      args.push("--volume", spec.cache_mount_path + ":/cache");
    }
    if (spec.workspace_base_path) {
      const hostDir = spec.workspace_base_path + "/" + spec.workspace_id;
      const mk = host.run(["mkdir", "-p", hostDir]);
      if (mk.exit_code !== 0) {
        throw new Error("failed to create workspace dir " + hostDir + ": " + mk.stderr);
      }
      args.push("--volume", hostDir + ":" + spec.work_dir);
    }
    args.push(spec.image, "sleep", "infinity");
    const created = host.run(args);
    if (created.exit_code !== 0) {
      throw new Error("failed to create container " + name + ": " + created.stderr);
    }
    const id = str(created.stdout).trim() || name;
    host.onFail(["container", "delete", "--force", id]);
    const started = host.run(["container", "start", id]);
    if (started.exit_code !== 0) {
      throw new Error("failed to start container " + name + " (" + id + "): " + started.stderr);
    }
    return { native_id: id, work_dir: spec.work_dir, username: "root", privilege: "root" };
  },
  destroy(handle) {
    host.run(["container", "delete", "--force", handle.native_id]);
  },
  destroyByID(id) {
    host.run(["container", "delete", "--force", id]);
  },
  recover(nativeID, workDir) {
    const res = host.run(["container", "inspect", nativeID]);
    if (res.exit_code !== 0) {
      throw new Error("container " + nativeID + " not found: " + res.stderr);
    }
    return { native_id: nativeID, work_dir: workDir, username: "root", privilege: "root" };
  },
  // The attachment is fixed when the container is created. Keep the hook so the
  // contract is explicit; no host command can add or remove it later.
  setNetwork(_handle, _enabled) {
  },
  execArgs(handle, cmd, workDir, env, opts) {
    const args = ["container", "exec"];
    if (opts.stdin) args.push("-i");
    if (opts.root) args.push("--user", "0");
    args.push("--workdir", workDir);
    for (const e of env) args.push("--env", e);
    args.push(handle.native_id);
    let command = cmd;
    if (opts.root && cmd.length >= 2 && cmd[0] === "tar" && /^-x/.test(cmd[1])) {
      command = ["tar", "-o", "-m", "--no-same-permissions", "--strip-components=1"].concat(cmd.slice(1));
    }
    return args.concat(command);
  },
  bootstrapCmd() {
    return ["git", "config", "--system", "--add", "safe.directory", "*"];
  }
};
module.exports = appleContainerPlugin;
