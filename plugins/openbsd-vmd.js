"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
function num(v, def) {
  return typeof v === "number" ? v : def;
}
const guestPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin";
const vmNamePrefix = "wsd-";
const masterDiskName = "disk-master.qcow2";
const diskFileName = "disk.qcow2";
const localNetworkPrefix = "100.64";
const localGuestLast = "3";
const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
function vmRoot() {
  return str(host.options["openbsd-vm-root"]) || "/var/wsd/vms/openbsd";
}
function masterImage() {
  return str(host.options["openbsd-master-image"]) || vmRoot() + "/" + masterDiskName;
}
function memory() {
  return str(host.options["openbsd-memory"]) || "1G";
}
function processors() {
  return num(host.options["openbsd-processors"], 1);
}
function loginUser() {
  return str(host.options["openbsd-login-user"]) || "root";
}
function sshKey() {
  return str(host.options["openbsd-ssh-key"]);
}
function sshPort() {
  return num(host.options["openbsd-ssh-port"], 22);
}
function guestOS() {
  return str(host.options["openbsd-guest-os"]) || "openbsd";
}
function hostEscalation() {
  return str(host.options["openbsd-escalation-command"]);
}
function guestEscalation() {
  return str(host.options["openbsd-guest-escalation-command"]);
}
function bootTimeoutMs() {
  return num(host.options["openbsd-boot-timeout"], 300) * 1e3;
}
function probeTimeoutSec() {
  return num(host.options["openbsd-ssh-probe-timeout"], 20);
}
function vmDiskDir(vmName) {
  return vmRoot() + "/" + vmName;
}
function diskPath(vmName) {
  return vmDiskDir(vmName) + "/" + diskFileName;
}
function escalate(argv) {
  const esc = hostEscalation();
  return esc ? [esc].concat(argv) : argv;
}
function validateWorkspaceID(id) {
  if (str(id).trim() === "") {
    throw new Error("workspace id is required");
  }
  if (id.indexOf("..") !== -1 || id.indexOf("/") !== -1 || id.indexOf("\\") !== -1 || !safeNameRE.test(id)) {
    throw new Error("invalid workspace id " + JSON.stringify(id));
  }
}
function validateVMName(vmName) {
  if (str(vmName).trim() === "") {
    throw new Error("openbsd VM name is required");
  }
  if (vmName.indexOf(vmNamePrefix) !== 0) {
    throw new Error("openbsd VM name " + JSON.stringify(vmName) + " must start with " + JSON.stringify(vmNamePrefix));
  }
  if (vmName.indexOf("..") !== -1 || vmName.indexOf("/") !== -1 || vmName.indexOf("\\") !== -1 || !safeNameRE.test(vmName)) {
    throw new Error("invalid openbsd VM name " + JSON.stringify(vmName));
  }
}
function parseVMID(output) {
  const m = /started vm ([0-9]+) successfully/.exec(output);
  if (!m) {
    throw new Error("no 'started vm N successfully' line in output: " + str(output).trim());
  }
  return parseInt(m[1], 10);
}
function parseVMIDByName(output, vmName) {
  const lines = output.split("\n");
  for (const line of lines) {
    const fields = line.trim().split(/\s+/);
    if (fields.length < 2) {
      continue;
    }
    if (fields[fields.length - 1] !== vmName) {
      continue;
    }
    const id = parseInt(fields[0], 10);
    if (!isNaN(id)) {
      return id;
    }
  }
  throw new Error("VM " + vmName + " is not running (no row in vmctl status)");
}
function localGuestIP(vmID) {
  return localNetworkPrefix + "." + vmID + "." + localGuestLast;
}
function vmctlStatusReachable(out) {
  return out.indexOf("STATE") !== -1 && out.indexOf("NAME") !== -1;
}
function isBenignVMError(stderr) {
  const s = str(stderr).toLowerCase();
  return s.indexOf("not found") !== -1 || s.indexOf("no such") !== -1 || s.indexOf("not running") !== -1 || s.indexOf("does not exist") !== -1 || s.indexOf("not a running") !== -1;
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
  let cmdStr = cmd.map((arg) => quoteArg(arg)).join(" ");
  if (env.length > 0) {
    cmdStr = env.map((assignment) => quoteArg(assignment)).join(" ") + " " + cmdStr;
  }
  if (workDir !== "") {
    cmdStr = "cd " + quoteArg(workDir) + " && " + cmdStr;
  }
  return cmdStr;
}
function remotePayload(escalation, cmd, workDir, env) {
  const inner = buildRemoteCommand(cmd, workDir, env);
  if (escalation !== "") {
    return escalation + " sh -c " + quoteArg(inner);
  }
  return inner;
}
function sshArgv(ip, port, user, keyPath, payload, connectTimeout) {
  const args = [
    "ssh",
    "-o",
    "StrictHostKeyChecking=no",
    "-o",
    "UserKnownHostsFile=/dev/null",
    "-o",
    "BatchMode=yes",
    "-o",
    "ConnectTimeout=" + connectTimeout,
    "-o",
    "LogLevel=ERROR"
  ];
  if (keyPath !== "") {
    args.push("-i", keyPath);
  }
  args.push("-p", String(port), user + "@" + ip, payload);
  return args;
}
function isFatalSSHProbeError(stderr) {
  const s = str(stderr).toLowerCase();
  return s.indexOf("permission denied (publickey") !== -1 || s.indexOf("permission denied (keyboard-interactive") !== -1 || s.indexOf("doas:") !== -1 || s.indexOf("sudo:") !== -1 || s.indexOf("operation not permitted") !== -1;
}
function handleFor(vmName, ip, workDir) {
  const user = loginUser();
  const privilege = user === "root" ? "root" : "user";
  const escalation = user === "root" ? "" : guestEscalation();
  return {
    native_id: vmName,
    work_dir: workDir,
    username: user,
    privilege,
    os: guestOS(),
    ip,
    port: sshPort(),
    key_path: sshKey(),
    escalation
  };
}
function waitForSSH(ip) {
  const deadline = Date.now() + bootTimeoutMs();
  const port = sshPort();
  const user = loginUser();
  const key = sshKey();
  const escalation = user === "root" ? "" : guestEscalation();
  for (; ; ) {
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for " + ip + " to appear in the ARP table");
    }
    const res = host.run(["arp", "-an"]);
    if (res.exit_code === 0 && str(res.stdout).indexOf(ip) !== -1) {
      break;
    }
    host.sleep(2e3);
  }
  for (; ; ) {
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for TCP " + ip + ":" + port + " to open");
    }
    const res = host.run(["nc", "-z", "-w", "3", ip, String(port)]);
    if (res.exit_code === 0) {
      break;
    }
    host.sleep(2e3);
  }
  const probePayload = remotePayload(escalation, ["true"], "", ["PATH=" + guestPath]);
  for (; ; ) {
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for SSH on " + ip + " to accept commands");
    }
    const res = host.run(sshArgv(ip, port, user, key, probePayload, probeTimeoutSec()));
    if (res.exit_code === 0) {
      return;
    }
    if (isFatalSSHProbeError(res.stderr)) {
      throw new Error("SSH to " + ip + " is reachable but the guest login/escalation is misconfigured: " + str(res.stderr).trim());
    }
    host.sleep(2e3);
  }
}
function destroyVM(vmName) {
  validateVMName(vmName);
  const stop = host.run(escalate(["vmctl", "stop", "-f", vmName]));
  if (stop.exit_code !== 0 && !isBenignVMError(stop.stderr)) {
    throw new Error("failed to stop VM " + vmName + ": " + stop.stderr);
  }
  const rm = host.run(escalate(["rm", "-rf", vmDiskDir(vmName)]));
  if (rm.exit_code !== 0) {
    throw new Error("failed to remove VM disk directory " + vmDiskDir(vmName) + ": " + rm.stderr);
  }
}
const openbsdVMDPlugin = {
  name: "openbsd-vmd",
  description: "OpenBSD vmd(8) virtual machines, driven through vmctl(8) and ssh(1)",
  version: "1.0.0",
  os: "openbsd",
  isolation: "vm",
  binaries: ["vmctl", "ssh", "arp", "nc", "mkdir", "rm", "ls", "test", "doas", "sudo"],
  options: [
    {
      flag: "openbsd-vm-root",
      type: "string",
      default: "/var/wsd/vms/openbsd",
      usage: "Directory under which each OpenBSD VM's disk directory is created"
    },
    {
      flag: "openbsd-master-image",
      type: "string",
      default: "",
      usage: "Path to the administrator-built golden qcow2 that workspaces are cloned from (default <vm-root>/disk-master.qcow2)"
    },
    {
      flag: "openbsd-memory",
      type: "string",
      default: "1G",
      usage: "Memory size passed to vmctl(8) -m for each VM, e.g. 1G"
    },
    {
      flag: "openbsd-processors",
      type: "int",
      default: "1",
      usage: "Number of processors passed to vmctl(8) -p for each VM (only passed when > 1)"
    },
    {
      flag: "openbsd-login-user",
      type: "string",
      default: "root",
      usage: "Guest account the daemon SSHes in as; use root or a dedicated user with doas/sudo"
    },
    {
      flag: "openbsd-ssh-key",
      type: "string",
      default: "/var/wsd/agent_ed25519",
      usage: "Host path to the SSH private key matching the public key baked into the master image"
    },
    {
      flag: "openbsd-ssh-port",
      type: "int",
      default: "22",
      usage: "Guest SSH port"
    },
    {
      flag: "openbsd-guest-os",
      type: "string",
      default: "openbsd",
      usage: "Operating system running inside the VM: openbsd, freebsd, or linux"
    },
    {
      flag: "openbsd-escalation-command",
      type: "string",
      default: "doas",
      usage: "Host privilege escalation command for vmctl(8)/rm, e.g. doas or sudo (empty runs directly)"
    },
    {
      flag: "openbsd-guest-escalation-command",
      type: "string",
      default: "doas",
      usage: "Privilege escalation command used inside the guest VM, e.g. doas or sudo (empty runs directly; ignored when the login user is root)"
    },
    {
      flag: "openbsd-boot-timeout",
      type: "duration",
      default: "5m",
      usage: "How long to wait for a freshly started VM to accept SSH"
    },
    {
      flag: "openbsd-ssh-probe-timeout",
      type: "duration",
      default: "20s",
      usage: "Bound on a single SSH readiness probe while waiting for a VM to boot"
    }
  ],
  init() {
    if (hostEscalation() !== "") {
      const res = host.run(escalate(["vmctl", "log", "brief"]));
      if (res.exit_code !== 0) {
        throw new Error("privilege escalator " + hostEscalation() + " is unavailable: " + res.stderr);
      }
    }
  },
  validate() {
    const key = sshKey();
    if (key === "") {
      throw new Error("openbsd VM ssh key path is required (-openbsd-ssh-key)");
    }
    if (host.run(["test", "-r", key]).exit_code !== 0) {
      throw new Error("openbsd VM ssh key " + key + " is not readable");
    }
    const master = masterImage();
    if (host.run(["test", "-f", master]).exit_code !== 0) {
      throw new Error("openbsd VM master image " + master + " is not a regular file");
    }
    if (host.run(["test", "-s", master]).exit_code !== 0) {
      throw new Error("openbsd VM master image " + master + " is empty");
    }
    const root = vmRoot();
    if (host.run(["mkdir", "-p", root]).exit_code !== 0) {
      throw new Error("openbsd VM root " + root + " is not creatable");
    }
    if (host.run(["test", "-w", root]).exit_code !== 0) {
      throw new Error("openbsd VM root " + root + " is not writable");
    }
    const status = host.run(escalate(["vmctl", "status"]));
    if (status.exit_code !== 0 && !vmctlStatusReachable(status.stdout)) {
      throw new Error("vmctl status failed (is vmd(8) running, and is escalation configured?): " + status.stderr);
    }
  },
  provision(spec) {
    const workspaceID = str(spec.workspace_id);
    validateWorkspaceID(workspaceID);
    const vmName = vmNamePrefix + workspaceID;
    const diskDir = vmDiskDir(vmName);
    const disk = diskPath(vmName);
    const mk = host.run(["mkdir", "-p", diskDir]);
    if (mk.exit_code !== 0) {
      throw new Error("failed to create VM directory " + diskDir + ": " + mk.stderr);
    }
    host.onFail(escalate(["rm", "-rf", diskDir]));
    const clone = host.run(escalate(["vmctl", "create", "-b", masterImage(), disk]));
    if (clone.exit_code !== 0) {
      throw new Error("failed to create VM disk from master " + masterImage() + ": " + clone.stderr);
    }
    host.onFail(escalate(["vmctl", "stop", "-f", vmName]));
    const startArgs = ["vmctl", "start", "-L", "-B", "disk", "-d", disk, "-m", memory()];
    if (processors() > 1) {
      startArgs.push("-p", String(processors()));
    }
    startArgs.push(vmName);
    const started = host.run(escalate(startArgs));
    if (started.exit_code !== 0) {
      throw new Error("failed to start VM " + vmName + ": " + started.stderr);
    }
    const vmID = parseVMID(str(started.stdout));
    const ip = localGuestIP(vmID);
    waitForSSH(ip);
    return handleFor(vmName, ip, spec.work_dir);
  },
  destroy(handle) {
    destroyVM(str(handle.native_id));
  },
  destroyByID(id) {
    destroyVM(id);
  },
  destroyWorkspaceByID(workspaceID) {
    validateWorkspaceID(workspaceID);
    destroyVM(vmNamePrefix + workspaceID);
  },
  recover(nativeID, workDir) {
    validateVMName(nativeID);
    if (host.run(["test", "-d", vmDiskDir(nativeID)]).exit_code !== 0) {
      throw new Error("VM " + nativeID + " not found");
    }
    const status = host.run(escalate(["vmctl", "status"]));
    if (status.exit_code !== 0 && !vmctlStatusReachable(status.stdout)) {
      throw new Error("failed to query vmctl status for VM " + nativeID + ": " + status.stderr);
    }
    const vmID = parseVMIDByName(str(status.stdout), nativeID);
    const ip = localGuestIP(vmID);
    return handleFor(nativeID, ip, workDir);
  },
  cleanupOrphans(keep) {
    const root = vmRoot();
    const ls = host.run(["ls", "-1", root]);
    if (ls.exit_code !== 0) {
      return 0;
    }
    const keepSet = {};
    for (const name of keep) {
      keepSet[name] = true;
    }
    let removed = 0;
    const lines = str(ls.stdout).split("\n");
    for (const raw of lines) {
      const name = raw.trim();
      if (name.indexOf(vmNamePrefix) !== 0 || keepSet[name]) {
        continue;
      }
      if (host.run(["test", "-f", root + "/" + name + "/" + diskFileName]).exit_code !== 0) {
        continue;
      }
      host.run(escalate(["vmctl", "stop", "-f", name]));
      if (host.run(escalate(["rm", "-rf", root + "/" + name])).exit_code === 0) {
        removed++;
      }
    }
    return removed;
  },
  execArgs(handle, cmd, workDir, env, _opts) {
    const fullEnv = ["PATH=" + guestPath].concat(env || []);
    const payload = remotePayload(str(handle.escalation), cmd, workDir, fullEnv);
    return sshArgv(str(handle.ip), num(handle.port, sshPort()), str(handle.username), str(handle.key_path), payload, 55);
  }
};
module.exports = openbsdVMDPlugin;
