/// <reference path="../../types/mcwsd-runtime.d.ts" />
import type { RuntimePlugin, ProvisionSpec, Handle, ExecArgsOptions } from "../../types/mcwsd-runtime";
"use strict";

/**
 * OpenBSD vmd(8) runtime plugin: provisions each workspace as a full virtual
 * machine cloned from an administrator-built golden image and reached over
 * SSH. This is the port of the former native openbsd.VMDProvisioner.
 *
 * A VM's filesystem is locked inside a disk image, so the daemon never writes
 * into it directly; the guest is a black box exposing only SSH. The lifecycle
 * maps onto vmctl(8) (clone/start/stop/status), and the transport maps onto
 * ssh(1) — both through the constrained `host.run` API.
 *
 * Options (the former --openbsd-* flags):
 *   --openbsd-vm-root                     base dir for per-workspace disk dirs
 *   --openbsd-master-image                golden qcow2 (default <vm-root>/disk-master.qcow2)
 *   --openbsd-memory / --openbsd-processors  vmctl -m / -p values
 *   --openbsd-login-user / --openbsd-ssh-key / --openbsd-ssh-port  guest SSH
 *   --openbsd-guest-os                    os reported on the handle (openbsd|freebsd|linux)
 *   --openbsd-escalation-command          host escalator for vmctl/rm (doas|sudo, "" = direct)
 *   --openbsd-guest-escalation-command    escalator wrapped around commands run inside the guest
 *   --openbsd-boot-timeout / --openbsd-ssh-probe-timeout  SSH readiness bounds
 */

function str(v: unknown): string {
  return typeof v === "string" ? v : v === undefined || v === null ? "" : String(v);
}

function num(v: unknown, def: number): number {
  return typeof v === "number" ? v : def;
}

// guestPath is exported for every command run in the guest. sshd starts a
// non-login shell, which on OpenBSD does not necessarily include the package
// prefix, so tools installed with pkg_add(1) would not be found.
const guestPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin";

const vmNamePrefix = "wsd-";
const masterDiskName = "disk-master.qcow2";
const diskFileName = "disk.qcow2";

// vmd(8) local-interface address formula: the guest's first local interface
// gets 100.64.<id>.3 (see vmctl(8) "LOCAL INTERFACES"). Only the guest side is
// reachable as the VM; .2 is the OpenBSD host's own end of the tap.
const localNetworkPrefix = "100.64";
const localGuestLast = "3";

const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

// --- options ---

function vmRoot(): string {
  return str(host.options["openbsd-vm-root"]) || "/var/wsd/vms/openbsd";
}
function masterImage(): string {
  return str(host.options["openbsd-master-image"]) || vmRoot() + "/" + masterDiskName;
}
function memory(): string {
  return str(host.options["openbsd-memory"]) || "1G";
}
function processors(): number {
  return num(host.options["openbsd-processors"], 1);
}
function loginUser(): string {
  return str(host.options["openbsd-login-user"]) || "root";
}
function sshKey(): string {
  return str(host.options["openbsd-ssh-key"]);
}
function sshPort(): number {
  return num(host.options["openbsd-ssh-port"], 22);
}
function guestOS(): string {
  return str(host.options["openbsd-guest-os"]) || "openbsd";
}
function hostEscalation(): string {
  return str(host.options["openbsd-escalation-command"]);
}
function guestEscalation(): string {
  return str(host.options["openbsd-guest-escalation-command"]);
}
// Durations arrive as seconds (float64) from the option binder.
function bootTimeoutMs(): number {
  return num(host.options["openbsd-boot-timeout"], 300) * 1000;
}
function probeTimeoutSec(): number {
  return num(host.options["openbsd-ssh-probe-timeout"], 20);
}

function vmDiskDir(vmName: string): string {
  return vmRoot() + "/" + vmName;
}
function diskPath(vmName: string): string {
  return vmDiskDir(vmName) + "/" + diskFileName;
}

// escalate prepends the host escalator (doas/sudo) to a host argv. vmctl(8)
// create/start/stop need root, and the disk directory removal is escalated for
// the same reason the native provisioner escalated removeJailRoot: the disk
// file was written by root (via the escalated vmctl create).
function escalate(argv: string[]): string[] {
  const esc = hostEscalation();
  return esc ? [esc].concat(argv) : argv;
}

// --- validation helpers (ported from the native provisioner) ---

function validateWorkspaceID(id: string): void {
  if (str(id).trim() === "") {
    throw new Error("workspace id is required");
  }
  if (id.indexOf("..") !== -1 || id.indexOf("/") !== -1 || id.indexOf("\\") !== -1 || !safeNameRE.test(id)) {
    throw new Error("invalid workspace id " + JSON.stringify(id));
  }
}

function validateVMName(vmName: string): void {
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

// parseVMID extracts the numeric VM ID from vmctl start's output; the ID keys
// the local-interface address formula.
function parseVMID(output: string): number {
  const m = /started vm ([0-9]+) successfully/.exec(output);
  if (!m) {
    throw new Error("no 'started vm N successfully' line in output: " + str(output).trim());
  }
  return parseInt(m[1], 10);
}

// parseVMIDByName extracts the numeric VM ID for vmName from `vmctl status`
// output. Each VM is one row with the name in the final column.
function parseVMIDByName(output: string, vmName: string): number {
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

function localGuestIP(vmID: number): string {
  return localNetworkPrefix + "." + vmID + "." + localGuestLast;
}

// vmctlStatusReachable reports whether out is the table header vmctl(8) prints
// once vmd(8) has answered. Its presence distinguishes "no VMs running"
// (benign, non-zero exit) from a real failure to reach vmd.
function vmctlStatusReachable(out: string): boolean {
  return out.indexOf("STATE") !== -1 && out.indexOf("NAME") !== -1;
}

// isBenignVMError reports whether a vmctl stop error means the VM was already
// stopped or gone, neither of which should block cleanup.
function isBenignVMError(stderr: string): boolean {
  const s = str(stderr).toLowerCase();
  return (
    s.indexOf("not found") !== -1 ||
    s.indexOf("no such") !== -1 ||
    s.indexOf("not running") !== -1 ||
    s.indexOf("does not exist") !== -1 ||
    s.indexOf("not a running") !== -1
  );
}

// --- SSH transport helpers (ported from SSHExecTransport) ---

// quoteArg shell-quotes an argument when it contains characters that would
// otherwise be interpreted by a shell.
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
// interpreted by the guest's shell rather than the local one. Env assignments
// are placed after the cd so they apply to the command, not to cd itself.
function buildRemoteCommand(cmd: string[], workDir: string, env: string[]): string {
  let cmdStr = cmd.map((arg) => quoteArg(arg)).join(" ");
  if (env.length > 0) {
    cmdStr = env.map((assignment) => quoteArg(assignment)).join(" ") + " " + cmdStr;
  }
  if (workDir !== "") {
    cmdStr = "cd " + quoteArg(workDir) + " && " + cmdStr;
  }
  return cmdStr;
}

// remotePayload wraps the inner payload in the guest escalation command
// (`doas sh -c '<inner>'`) when one is configured. The wrap is `doas sh -c`
// (not `doas <inner>`) so a compound inner command (cd/&&) is still
// interpreted by a shell, and env assignments apply inside the escalated shell.
function remotePayload(escalation: string, cmd: string[], workDir: string, env: string[]): string {
  const inner = buildRemoteCommand(cmd, workDir, env);
  if (escalation !== "") {
    return escalation + " sh -c " + quoteArg(inner);
  }
  return inner;
}

// sshArgv assembles the ssh(1) argument vector. The remote payload is a single
// argument; ssh concatenates its arguments and hands the result to the remote
// user's login shell, so a payload containing && or ; is still interpreted
// remotely (as intended) but never by the local shell.
function sshArgv(
  ip: string,
  port: number,
  user: string,
  keyPath: string,
  payload: string,
  connectTimeout: number,
): string[] {
  const args = [
    "ssh",
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "BatchMode=yes",
    "-o", "ConnectTimeout=" + connectTimeout,
    "-o", "LogLevel=ERROR",
  ];
  if (keyPath !== "") {
    args.push("-i", keyPath);
  }
  args.push("-p", String(port), user + "@" + ip, payload);
  return args;
}

// isFatalSSHProbeError reports whether a failed readiness probe failed for a
// reason retrying cannot fix: SSH rejected the key, or the guest escalation
// command (doas/sudo) refused. Transient conditions ("connection refused") keep
// being retried.
function isFatalSSHProbeError(stderr: string): boolean {
  const s = str(stderr).toLowerCase();
  return (
    s.indexOf("permission denied (publickey") !== -1 ||
    s.indexOf("permission denied (keyboard-interactive") !== -1 ||
    s.indexOf("doas:") !== -1 ||
    s.indexOf("sudo:") !== -1 ||
    s.indexOf("operation not permitted") !== -1
  );
}

// handleFor builds the per-workspace handle. Beyond the four fixed fields it
// carries the SSH connection details (ip/port/key/escalation) that execArgs
// needs, plus the guest os so the handle reports the right package-manager OS.
function handleFor(vmName: string, ip: string, workDir: string): Handle {
  const user = loginUser();
  const privilege: "root" | "user" = user === "root" ? "root" : "user";
  const escalation = user === "root" ? "" : guestEscalation();
  return {
    native_id: vmName,
    work_dir: workDir,
    username: user,
    privilege: privilege,
    os: guestOS(),
    ip: ip,
    port: sshPort(),
    key_path: sshKey(),
    escalation: escalation,
  };
}

// waitForSSH polls until the guest at ip answers on SSH, or bootTimeout
// elapses. It proceeds in three stages so a failure is reported precisely: the
// address appearing in the ARP table, the TCP port opening, then a real SSH
// command succeeding (which also proves the key, login user, and guest
// escalation all work).
function waitForSSH(ip: string): void {
  const deadline = Date.now() + bootTimeoutMs();
  const port = sshPort();
  const user = loginUser();
  const key = sshKey();
  const escalation = user === "root" ? "" : guestEscalation();

  // Stage 1: the address must appear on the host's ARP table.
  for (;;) {
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for " + ip + " to appear in the ARP table");
    }
    const res = host.run(["arp", "-an"]);
    if (res.exit_code === 0 && str(res.stdout).indexOf(ip) !== -1) {
      break;
    }
    host.sleep(2000);
  }

  // Stage 2: the SSH port must accept a TCP connection.
  for (;;) {
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for TCP " + ip + ":" + port + " to open");
    }
    const res = host.run(["nc", "-z", "-w", "3", ip, String(port)]);
    if (res.exit_code === 0) {
      break;
    }
    host.sleep(2000);
  }

  // Stage 3: a real SSH command must succeed.
  const probePayload = remotePayload(escalation, ["true"], "", ["PATH=" + guestPath]);
  for (;;) {
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
    host.sleep(2000);
  }
}

// destroyVM force-stops a VM by name and removes its disk directory. It is
// idempotent: a VM that is already stopped or gone is not an error.
function destroyVM(vmName: string): void {
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

const openbsdVMDPlugin: RuntimePlugin = {
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
      usage: "Directory under which each OpenBSD VM's disk directory is created",
    },
    {
      flag: "openbsd-master-image",
      type: "string",
      default: "",
      usage: "Path to the administrator-built golden qcow2 that workspaces are cloned from (default <vm-root>/disk-master.qcow2)",
    },
    {
      flag: "openbsd-memory",
      type: "string",
      default: "1G",
      usage: "Memory size passed to vmctl(8) -m for each VM, e.g. 1G",
    },
    {
      flag: "openbsd-processors",
      type: "int",
      default: "1",
      usage: "Number of processors passed to vmctl(8) -p for each VM (only passed when > 1)",
    },
    {
      flag: "openbsd-login-user",
      type: "string",
      default: "root",
      usage: "Guest account the daemon SSHes in as; use root or a dedicated user with doas/sudo",
    },
    {
      flag: "openbsd-ssh-key",
      type: "string",
      default: "/var/wsd/agent_ed25519",
      usage: "Host path to the SSH private key matching the public key baked into the master image",
    },
    {
      flag: "openbsd-ssh-port",
      type: "int",
      default: "22",
      usage: "Guest SSH port",
    },
    {
      flag: "openbsd-guest-os",
      type: "string",
      default: "openbsd",
      usage: "Operating system running inside the VM: openbsd, freebsd, or linux",
    },
    {
      flag: "openbsd-escalation-command",
      type: "string",
      default: "doas",
      usage: "Host privilege escalation command for vmctl(8)/rm, e.g. doas or sudo (empty runs directly)",
    },
    {
      flag: "openbsd-guest-escalation-command",
      type: "string",
      default: "doas",
      usage: "Privilege escalation command used inside the guest VM, e.g. doas or sudo (empty runs directly; ignored when the login user is root)",
    },
    {
      flag: "openbsd-boot-timeout",
      type: "duration",
      default: "5m",
      usage: "How long to wait for a freshly started VM to accept SSH",
    },
    {
      flag: "openbsd-ssh-probe-timeout",
      type: "duration",
      default: "20s",
      usage: "Bound on a single SSH readiness probe while waiting for a VM to boot",
    },
  ],

  init(): void {
    if (hostEscalation() !== "") {
      const res = host.run(escalate(["vmctl", "log", "brief"]));
      if (res.exit_code !== 0) {
        throw new Error("privilege escalator " + hostEscalation() + " is unavailable: " + res.stderr);
      }
    }
  },

  validate(): void {
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

    // A harmless privileged query verifies both that escalation works and
    // that vmd(8) is up. vmctl(8) exits non-zero when there are no VMs to
    // list, which is the normal startup state; the table header's presence
    // proves the control connection worked.
    const status = host.run(escalate(["vmctl", "status"]));
    if (status.exit_code !== 0 && !vmctlStatusReachable(status.stdout)) {
      throw new Error("vmctl status failed (is vmd(8) running, and is escalation configured?): " + status.stderr);
    }
  },

  provision(spec: ProvisionSpec): Handle {
    const workspaceID = str(spec.workspace_id);
    validateWorkspaceID(workspaceID);

    const vmName = vmNamePrefix + workspaceID;
    const diskDir = vmDiskDir(vmName);
    const disk = diskPath(vmName);

    const mk = host.run(["mkdir", "-p", diskDir]);
    if (mk.exit_code !== 0) {
      throw new Error("failed to create VM directory " + diskDir + ": " + mk.stderr);
    }
    // Undo the disk directory if anything downstream fails.
    host.onFail(escalate(["rm", "-rf", diskDir]));

    // Copy-on-write clone of the golden image. -b (not -i) makes this
    // near-instant regardless of the master's size.
    const clone = host.run(escalate(["vmctl", "create", "-b", masterImage(), disk]));
    if (clone.exit_code !== 0) {
      throw new Error("failed to create VM disk from master " + masterImage() + ": " + clone.stderr);
    }
    // Undo the VM (if it gets started) before the disk removal above.
    host.onFail(escalate(["vmctl", "stop", "-f", vmName]));

    // Start with a local interface (-L) so vmd(8) runs DHCP and assigns a
    // predictable address derived from the VM ID. -B disk boots from the
    // cloned disk. -p is only passed when > 1 (OpenBSD 7.9 and earlier lack it).
    const startArgs = ["vmctl", "start", "-L", "-B", "disk", "-d", disk, "-m", memory()];
    if (processors() > 1) {
      startArgs.push("-p", String(processors()));
    }
    startArgs.push(vmName);
    const started = host.run(escalate(startArgs));
    if (started.exit_code !== 0) {
      throw new Error("failed to start VM " + vmName + ": " + started.stderr);
    }

    // vmctl(8) prints "started vm N successfully" on stderr, not stdout, so
    // the VM ID is parsed from the combined output (the native provisioner
    // read CombinedOutput for the same reason).
    const vmID = parseVMID(str(started.stdout) + "\n" + str(started.stderr));
    const ip = localGuestIP(vmID);

    waitForSSH(ip);

    return handleFor(vmName, ip, spec.work_dir);
  },

  destroy(handle: Handle): void {
    destroyVM(str(handle.native_id));
  },

  destroyByID(id: string): void {
    destroyVM(id);
  },

  destroyWorkspaceByID(workspaceID: string): void {
    validateWorkspaceID(workspaceID);
    destroyVM(vmNamePrefix + workspaceID);
  },

  recover(nativeID: string, workDir: string): Handle {
    validateVMName(nativeID);
    if (host.run(["test", "-d", vmDiskDir(nativeID)]).exit_code !== 0) {
      throw new Error("VM " + nativeID + " not found");
    }
    // The guest address derives from the numeric vmd(8) ID, which is assigned
    // at start time and is not part of the VM name; query the running table.
    const status = host.run(escalate(["vmctl", "status"]));
    if (status.exit_code !== 0 && !vmctlStatusReachable(status.stdout)) {
      throw new Error("failed to query vmctl status for VM " + nativeID + ": " + status.stderr);
    }
    const vmID = parseVMIDByName(str(status.stdout), nativeID);
    const ip = localGuestIP(vmID);
    return handleFor(nativeID, ip, workDir);
  },

  cleanupOrphans(keep: string[]): number {
    const root = vmRoot();
    const ls = host.run(["ls", "-1", root]);
    if (ls.exit_code !== 0) {
      return 0;
    }
    const keepSet: Record<string, boolean> = {};
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
      // Not one of ours (no per-workspace disk); leave it alone.
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

  execArgs(handle: Handle, cmd: string[], workDir: string, env: string[], _opts: ExecArgsOptions): string[] {
    const fullEnv = ["PATH=" + guestPath].concat(env || []);
    const payload = remotePayload(str(handle.escalation), cmd, workDir, fullEnv);
    return sshArgv(str(handle.ip), num(handle.port, sshPort()), str(handle.username), str(handle.key_path), payload, 55);
  },
};

module.exports = openbsdVMDPlugin;
