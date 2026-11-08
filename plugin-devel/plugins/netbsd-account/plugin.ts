/// <reference path="../../types/mcwsd-runtime.d.ts" />
import type { RuntimePlugin, ProvisionSpec, Handle, ExecArgsOptions } from "../../types/mcwsd-runtime";
"use strict";

/**
 * NetBSD account runtime plugin: provisions each workspace as a local user
 * account on the host instead of a jail, VM, or container. This is the first
 * "process"-isolation runtime and the first whose transport *drops* privileges
 * (to the workspace account) rather than escalating.
 *
 * Model:
 *   - The daemon runs non-root (as _wsd) and reaches root only through the
 *     configured escalator (sudo/doas) for the account lifecycle commands
 *     (useradd/groupadd/userdel/...).
 *   - The workspace account has no escalation of its own. Every exec -- user
 *     commands and core's privileged file I/O alike -- runs as the account via
 *     "<escalator> -u <account> sh -c ...". Core's opts.root is deliberately
 *     ignored: in this model there is no sandbox root to act as, and writing
 *     files as the account is what keeps them editable by the workspace.
 *   - The account's home directory IS the workspace. Core's fixed in-runtime
 *     path "/workspace" is remapped onto the account's home, so the repo lands
 *     at <home>/repo and is owned by the account.
 *
 * Options:
 *   --account-home-base     base dir for workspace accounts (default /home)
 *   --account-login-class   NetBSD login class applied to accounts (empty = default)
 *   --account-shell         login shell for accounts (default /sbin/nologin)
 *   --escalation-command    sudo or doas (default sudo; empty is rejected)
 *
 * Security posture (documented in NETBSD-SETUP.md): process isolation gives no
 * filesystem/network/uid sandbox -- the account shares the host's network, /tmp,
 * and readable /etc/passwd. Login classes bound resources only, and only when
 * the escalator applies setusercontext(3). Workspace accounts are not granted
 * any escalation.
 */

function str(v: unknown): string {
  return typeof v === "string" ? v : v === undefined || v === null ? "" : String(v);
}

// workspacePath is exported for every command run as the workspace account.
// NetBSD's pkgsrc binaries live under /usr/pkg, and sudo's secure_path does not
// include them, so a full PATH is set explicitly on the remote shell.
const workspacePath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/pkg/sbin:/usr/pkg/bin:/usr/local/bin";

// accountNameRE matches the exact shape core generates (generateWorkspaceID ->
// "ws-" + 9 lowercase hex), so cleanupOrphans only ever touches our accounts.
const accountNameRE = /^ws-[0-9a-f]{9}$/;
const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

// core's fixed in-runtime workspace path, remapped onto the account home.
const workspacePrefix = "/workspace";

// --- options ---

function homeBase(): string {
  return str(host.options["account-home-base"]) || "/home";
}
function loginClassOpt(): string {
  return str(host.options["account-login-class"]);
}
function accountGroup(): string {
  return str(host.options["account-group"]) || "wsd";
}
function accountShell(): string {
  return str(host.options["account-shell"]) || "/sbin/nologin";
}
function escalator(): string {
  return str(host.options["escalation-command"]);
}

// escalate prepends the host escalator (sudo/doas) to a host argv. Account
// lifecycle commands (useradd/groupadd/userdel/...) require root and the
// daemon is non-root, so they always go through the escalator.
function escalate(argv: string[]): string[] {
  const esc = escalator();
  return esc ? [esc].concat(argv) : argv;
}

// dropAs runs argv as the given user: "<escalator> -u <user> ...". Both sudo
// and doas accept -u with the same shape.
function dropAs(user: string, argv: string[]): string[] {
  const esc = escalator();
  return esc ? [esc, "-u", user].concat(argv) : argv;
}

// --- validation ---

function validateWorkspaceID(id: string): void {
  if (str(id).trim() === "") {
    throw new Error("workspace id is required");
  }
  if (id.indexOf("..") !== -1 || id.indexOf("/") !== -1 || id.indexOf("\\") !== -1 || !safeNameRE.test(id)) {
    throw new Error("invalid workspace id " + JSON.stringify(id));
  }
}

function validateAccountName(name: string): void {
  validateWorkspaceID(name);
  if (name.indexOf("ws-") !== 0) {
    throw new Error("account name " + JSON.stringify(name) + " must start with \"ws-\"");
  }
}

function isWorkspaceAccount(name: string): boolean {
  return accountNameRE.test(name);
}

function isBenignUserError(stderr: string): boolean {
  const s = str(stderr).toLowerCase();
  return s.indexOf("no such user") !== -1 || s.indexOf("does not exist") !== -1;
}

function isBenignGroupError(stderr: string): boolean {
  const s = str(stderr).toLowerCase();
  return s.indexOf("no such group") !== -1 || s.indexOf("does not exist") !== -1;
}

// --- path mapping ---

// remapPath translates core's fixed in-runtime "/workspace" onto the account's
// home directory, so "/workspace/repo" resolves to "<home>/repo". It is applied
// to both the working directory and to path arguments inside the command argv
// (CopyDirIn's mkdir/tar, ReadFile/WriteFile's cat/chmod, ...).
function remapPath(p: string, home: string): string {
  if (p === workspacePrefix) {
    return home;
  }
  if (p.indexOf(workspacePrefix + "/") === 0) {
    return home + p.slice(workspacePrefix.length);
  }
  return p;
}

// --- transport (shell quoting, ported from the openbsd-vmd plugin) ---

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
// interpreted by the workspace account's shell (the outer "sh -c" we hand to
// the escalator) rather than the daemon's shell.
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

// --- handle ---

function handleFor(account: string, workDir: string): Handle {
  return {
    native_id: account,
    work_dir: workDir,
    username: account,
    privilege: "user",
    home: homeBase() + "/" + account,
  };
}

// --- lifecycle ---

function destroyAccount(account: string, home: string): void {
  validateAccountName(account);

  const ud = host.run(escalate(["userdel", "-r", account]));
  if (ud.exit_code !== 0 && !isBenignUserError(ud.stderr)) {
    throw new Error("failed to remove account " + account + ": " + str(ud.stderr).trim());
  }

  const gd = host.run(escalate(["groupdel", account]));
  if (gd.exit_code !== 0 && !isBenignGroupError(gd.stderr)) {
    throw new Error("failed to remove group " + account + ": " + str(gd.stderr).trim());
  }

  // userdel -r removes the home, but a partially provisioned account (useradd
  // failed after the home was created) has no passwd entry to clean up.
  host.run(escalate(["rm", "-rf", home]));
}

const netbsdAccountPlugin: RuntimePlugin = {
  name: "netbsd-account",
  description: "NetBSD workspace accounts (process isolation), driven through useradd(8) and sudo/doas",
  version: "1.0.0",
  os: "netbsd",
  isolation: "process",
  binaries: [
    "sudo", "doas", "useradd", "userdel", "groupadd", "groupdel", "userinfo",
    "chown", "rm", "mkdir", "cat", "test", "grep", "sh",
  ],
  options: [
    {
      flag: "account-home-base",
      type: "string",
      default: "/home",
      usage: "Base directory under which workspace accounts get their home directories",
    },
    {
      flag: "account-login-class",
      type: "string",
      default: "",
      usage: "NetBSD login class applied to workspace accounts (must be defined in /etc/login.conf)",
    },
    {
      flag: "account-group",
      type: "string",
      default: "wsd",
      usage: "Shared secondary group workspace accounts join; also authorizes the daemon's down-escalation via sudoers (%<group>)",
    },
    {
      flag: "account-shell",
      type: "string",
      default: "/sbin/nologin",
      usage: "Login shell assigned to workspace accounts",
    },
    {
      flag: "escalation-command",
      type: "string",
      default: "sudo",
      usage: "Privilege escalation command for account lifecycle and running commands as the workspace account (sudo or doas)",
    },
  ],

  validate(): void {
    const esc = escalator();
    if (esc === "") {
      throw new Error("netbsd-account requires --escalation-command (sudo or doas); the daemon must run non-root");
    }
    // /etc/master.passwd is root-only on NetBSD, so reading it proves the
    // escalator actually reaches root (userinfo alone would not).
    const res = host.run([esc, "test", "-r", "/etc/master.passwd"]);
    if (res.exit_code !== 0) {
      throw new Error("escalator " + esc + " cannot reach root (test -r /etc/master.passwd): " + str(res.stderr).trim());
    }

    const cls = loginClassOpt();
    if (cls !== "") {
      if (!/^[A-Za-z0-9_.-]+$/.test(cls)) {
        throw new Error("invalid login class name " + JSON.stringify(cls));
      }
      const g = host.run(["grep", "-q", "^" + cls + ":", "/etc/login.conf"]);
      if (g.exit_code !== 0) {
        throw new Error("login class " + cls + " is not defined in /etc/login.conf");
      }
    }

    // The shared group must exist so accounts can be added to it with useradd
    // -G, and so the sudoers runas rule "(%<group>)" can authorize the
    // daemon's down-escalation. Create it once in setup (see NETBSD-SETUP.md).
    const grp = accountGroup();
    const gg = host.run(["grep", "-q", "^" + grp + ":", "/etc/group"]);
    if (gg.exit_code !== 0) {
      throw new Error("shared group " + grp + " does not exist (run: groupadd " + grp + ")");
    }
  },

  provision(spec: ProvisionSpec): Handle {
    const workspaceID = str(spec.workspace_id);
    validateWorkspaceID(workspaceID);

    const account = workspaceID;
    const home = homeBase() + "/" + account;
    const shell = accountShell();
    const cls = loginClassOpt();

    const ga = host.run(escalate(["groupadd", account]));
    if (ga.exit_code !== 0) {
      throw new Error("failed to create group " + account + ": " + str(ga.stderr).trim());
    }
    host.onFail(escalate(["groupdel", account]));

    const ua = ["useradd", "-d", home, "-m", "-M", "700", "-g", account, "-G", accountGroup(), "-s", shell];
    if (cls !== "") {
      ua.push("-L", cls);
    }
    ua.push(account);
    const u = host.run(escalate(ua));
    if (u.exit_code !== 0) {
      throw new Error("failed to create account " + account + ": " + str(u.stderr).trim());
    }
    host.onFail(escalate(["userdel", "-r", account]));
    host.onFail(escalate(["rm", "-rf", home]));

    // Belt-and-suspenders: useradd -m -M 700 should already do this, but an
    // admin-built skeleton or a class override could leave ownership off.
    const ch = host.run(escalate(["chown", "-R", account + ":" + account, home]));
    if (ch.exit_code !== 0) {
      host.log(1, "chown home failed for " + account + ": " + str(ch.stderr).trim());
    }

    return handleFor(account, str(spec.work_dir));
  },

  destroy(handle: Handle): void {
    destroyAccount(str(handle.native_id), str(handle.home));
  },

  destroyByID(id: string): void {
    destroyAccount(id, homeBase() + "/" + id);
  },

  destroyWorkspaceByID(workspaceID: string): void {
    validateWorkspaceID(workspaceID);
    destroyAccount(workspaceID, homeBase() + "/" + workspaceID);
  },

  recover(nativeID: string, workDir: string): Handle {
    validateAccountName(nativeID);
    const res = host.run(escalate(["userinfo", nativeID]));
    if (res.exit_code !== 0) {
      throw new Error("account " + nativeID + " not found: " + str(res.stderr).trim());
    }
    return handleFor(nativeID, workDir);
  },

  cleanupOrphans(keep: string[]): number {
    const keepSet: Record<string, boolean> = {};
    for (const name of keep) {
      keepSet[name] = true;
    }
    const res = host.run(["cat", "/etc/passwd"]);
    if (res.exit_code !== 0) {
      return 0;
    }
    let removed = 0;
    const lines = str(res.stdout).split("\n");
    for (const raw of lines) {
      const name = raw.split(":")[0].trim();
      if (!isWorkspaceAccount(name) || keepSet[name]) {
        continue;
      }
      try {
        destroyAccount(name, homeBase() + "/" + name);
        removed++;
      } catch (e) {
        host.log(1, "cleanupOrphans failed for " + name + ": " + str(e));
      }
    }
    return removed;
  },

  execArgs(handle: Handle, cmd: string[], workDir: string, env: string[], _opts: ExecArgsOptions): string[] {
    const user = str(handle.username);
    const home = str(handle.home);

    const mappedCmd = (cmd || []).map((a) => remapPath(a, home));
    const mappedWorkDir = remapPath(workDir, home);
    const fullEnv = ["PATH=" + workspacePath, "HOME=" + home].concat(env || []);
    const payload = buildRemoteCommand(mappedCmd, mappedWorkDir, fullEnv);
    return dropAs(user, ["sh", "-c", payload]);
  },
};

module.exports = netbsdAccountPlugin;
