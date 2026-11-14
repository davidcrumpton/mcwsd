"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
const workspacePath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/pkg/sbin:/usr/pkg/bin:/usr/local/bin";
const accountNameRE = /^(ac-[0-9a-f]{12}|ws-[0-9a-f]{9,12})$/;
const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const workspacePrefix = "/workspace";
function homeBase() {
  return str(host.options["account-home-base"]) || "/home";
}
function loginClassOpt() {
  return str(host.options["account-login-class"]);
}
function accountGroup() {
  return str(host.options["account-group"]) || "wsd";
}
function accountShell() {
  return str(host.options["account-shell"]) || "/sbin/nologin";
}
function escalator() {
  return str(host.options["escalation-command"]);
}
function escalate(argv) {
  const esc = escalator();
  return esc ? [esc].concat(argv) : argv;
}
function dropAs(user, argv) {
  const esc = escalator();
  return esc ? [esc, "-u", user].concat(argv) : argv;
}
function validateWorkspaceID(id) {
  if (str(id).trim() === "") {
    throw new Error("workspace id is required");
  }
  if (id.indexOf("..") !== -1 || id.indexOf("/") !== -1 || id.indexOf("\\") !== -1 || !safeNameRE.test(id)) {
    throw new Error("invalid workspace id " + JSON.stringify(id));
  }
}
function validateAccountName(name) {
  validateWorkspaceID(name);
  if (!isWorkspaceAccount(name)) {
    throw new Error("account name " + JSON.stringify(name) + " is not managed by this plugin");
  }
}
function isWorkspaceAccount(name) {
  return accountNameRE.test(name);
}
function isBenignUserError(stderr) {
  const s = str(stderr).toLowerCase();
  return s.indexOf("no such user") !== -1 || s.indexOf("does not exist") !== -1;
}
function isBenignGroupError(stderr) {
  const s = str(stderr).toLowerCase();
  return s.indexOf("no such group") !== -1 || s.indexOf("does not exist") !== -1;
}
function remapPath(p, home) {
  if (p === workspacePrefix) {
    return home;
  }
  if (p.indexOf(workspacePrefix + "/") === 0) {
    return home + p.slice(workspacePrefix.length);
  }
  return p;
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
function handleFor(account, workDir) {
  const home = homeBase() + "/" + account;
  return {
    native_id: account,
    work_dir: remapPath(workDir, home),
    username: account,
    privilege: "user",
    home
  };
}
function destroyAccount(account, home) {
  validateAccountName(account);
  const ud = host.run(escalate(["userdel", "-r", account]));
  if (ud.exit_code !== 0 && !isBenignUserError(ud.stderr)) {
    throw new Error("failed to remove account " + account + ": " + str(ud.stderr).trim());
  }
  const gd = host.run(escalate(["groupdel", account]));
  if (gd.exit_code !== 0 && !isBenignGroupError(gd.stderr)) {
    throw new Error("failed to remove group " + account + ": " + str(gd.stderr).trim());
  }
  host.run(escalate(["rm", "-rf", home]));
}
const netbsdAccountPlugin = {
  name: "netbsd-account",
  description: "NetBSD workspace accounts (process isolation), driven through useradd(8) and sudo/doas",
  version: "1.0.0",
  os: "netbsd",
  isolation: "process",
  binaries: [
    "sudo",
    "doas",
    "useradd",
    "userdel",
    "groupadd",
    "groupdel",
    "userinfo",
    "chown",
    "rm",
    "mkdir",
    "cat",
    "test",
    "grep",
    "sh"
  ],
  options: [
    {
      flag: "account-home-base",
      type: "string",
      default: "/home",
      usage: "Base directory under which workspace accounts get their home directories"
    },
    {
      flag: "account-login-class",
      type: "string",
      default: "",
      usage: "NetBSD login class applied to workspace accounts (must be defined in /etc/login.conf)"
    },
    {
      flag: "account-group",
      type: "string",
      default: "wsd",
      usage: "Shared secondary group workspace accounts join; also authorizes the daemon's down-escalation via sudoers (%<group>)"
    },
    {
      flag: "account-shell",
      type: "string",
      default: "/sbin/nologin",
      usage: "Login shell assigned to workspace accounts"
    },
    {
      flag: "escalation-command",
      type: "string",
      default: "sudo",
      usage: "Privilege escalation command for account lifecycle and running commands as the workspace account (sudo or doas)"
    }
  ],
  validate() {
    const esc = escalator();
    if (esc === "") {
      throw new Error("netbsd-account requires --escalation-command (sudo or doas); the daemon must run non-root");
    }
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
    const grp = accountGroup();
    const gg = host.run(["grep", "-q", "^" + grp + ":", "/etc/group"]);
    if (gg.exit_code !== 0) {
      throw new Error("shared group " + grp + " does not exist (run: groupadd " + grp + ")");
    }
  },
  provision(spec) {
    const account = str(spec.instance_id);
    if (account === "") {
      throw new Error("netbsd-account requires core to provide a non-empty instance_id");
    }
    validateAccountName(account);
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
    const ch = host.run(escalate(["chown", "-R", account + ":" + account, home]));
    if (ch.exit_code !== 0) {
      host.log(1, "chown home failed for " + account + ": " + str(ch.stderr).trim());
    }
    return handleFor(account, str(spec.work_dir));
  },
  destroy(handle) {
    destroyAccount(str(handle.native_id), str(handle.home));
  },
  destroyByID(id) {
    destroyAccount(id, homeBase() + "/" + id);
  },
  recover(nativeID, workDir) {
    validateAccountName(nativeID);
    const res = host.run(escalate(["userinfo", nativeID]));
    if (res.exit_code !== 0) {
      throw new Error("account " + nativeID + " not found: " + str(res.stderr).trim());
    }
    return handleFor(nativeID, workDir);
  },
  cleanupOrphans(keep) {
    const keepSet = {};
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
  execArgs(handle, cmd, workDir, env, _opts) {
    const user = str(handle.username);
    const home = str(handle.home);
    const mappedCmd = (cmd || []).map((a) => remapPath(a, home));
    const mappedWorkDir = remapPath(workDir, home);
    const fullEnv = ["PATH=" + workspacePath, "HOME=" + home].concat(env || []);
    const payload = buildRemoteCommand(mappedCmd, mappedWorkDir, fullEnv);
    return dropAs(user, ["sh", "-c", payload]);
  }
};
module.exports = netbsdAccountPlugin;
