/// <reference path="../../types/mcwsd-runtime.d.ts" />
import type { RuntimePlugin, ProvisionSpec, Handle, ExecArgsOptions } from "../../types/mcwsd-runtime";
"use strict";

/**
 * Apple `container` runtime plugin: drives Apple's OCI container CLI on macOS.
 *
 * The CLI is intentionally Docker-shaped, so this port follows the docker and
 * podman plugins closely:
 *   - pull, create, start, and delete are separate commands;
 *   - exec takes `--workdir`, `--env`, `--user`, and `-i`;
 *   - the workspace directory is a host bind mount.
 *
 * Apple container fixes a container's network attachments when the container is
 * created. Unlike docker/podman there is no `network connect`/`disconnect`, so
 * setNetwork() cannot change an existing container. `--container-network`
 * therefore selects the attachment at provision time (default "default"; set it
 * to "none" to create network-less workspaces). The daemon's network requests
 * are still accepted, but once the attachment is chosen it is left as-is.
 *
 * Options:
 *   --container-network   network attached at create time (default "default").
 */

function str(v: unknown): string {
  return typeof v === "string" ? v : v === undefined || v === null ? "" : String(v);
}

function networkName(): string {
  return str(host.options["container-network"]) || "default";
}

function containerName(workspaceID: string): string {
  return "wsd-" + workspaceID;
}

const appleContainerPlugin: RuntimePlugin = {
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
      usage:
        "Apple container network attached at create time (use \"none\" for no network; attachment cannot be changed later)",
    },
  ],

  provision(spec: ProvisionSpec): Handle {
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
      networkName(),
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

  destroy(handle: Handle): void {
    host.run(["container", "delete", "--force", handle.native_id]);
  },

  destroyByID(id: string): void {
    host.run(["container", "delete", "--force", id]);
  },

  recover(nativeID: string, workDir: string): Handle {
    const res = host.run(["container", "inspect", nativeID]);
    if (res.exit_code !== 0) {
      throw new Error("container " + nativeID + " not found: " + res.stderr);
    }
    return { native_id: nativeID, work_dir: workDir, username: "root", privilege: "root" };
  },

  // The attachment is fixed when the container is created. Keep the hook so the
  // contract is explicit; no host command can add or remove it later.
  setNetwork(_handle: Handle, _enabled: boolean): void {},

  execArgs(handle: Handle, cmd: string[], workDir: string, env: string[], opts: ExecArgsOptions): string[] {
    const args = ["container", "exec"];
    if (opts.stdin) args.push("-i");
    if (opts.root) args.push("--user", "0");
    args.push("--workdir", workDir);
    for (const e of env) args.push("--env", e);
    args.push(handle.native_id);

    // Core copies the repo in by streaming a tar archive rooted at "." to
    // `tar -xf -` as root. On Apple container's virtiofs workspace mount the
    // guest root cannot restore the archive's host uid/gid, mtime, or mode on
    // the mount root, so tar exits non-zero. Skip those restores and strip the
    // "." entry; these flags work with both GNU tar (buildpack-deps) and
    // BusyBox tar (alpine), unlike GNU's --no-overwrite-dir.
    let command = cmd;
    if (opts.root && cmd.length >= 2 && cmd[0] === "tar" && /^-x/.test(cmd[1])) {
      command = ["tar", "-o", "-m", "--no-same-permissions", "--strip-components=1"].concat(cmd.slice(1));
    }
    return args.concat(command);
  },

  bootstrapCmd(): string[] | null {
    // The workspace repo is a host bind mount, and git inside the container may
    // run as a different uid than the host owner.
    return ["git", "config", "--system", "--add", "safe.directory", "*"];
  },
};

module.exports = appleContainerPlugin;
