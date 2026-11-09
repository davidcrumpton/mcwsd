"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
function num(v, def) {
  return typeof v === "number" ? v : def;
}
function logInfo(msg) {
  host.log(2, msg);
}
function logWarn(msg) {
  host.log(1, msg);
}
const guestPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
const hostnamePrefix = "wsd-";
const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const ipv4RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
function apiURL() {
  return str(host.options["proxmox-api-url"]);
}
function nodeName() {
  return str(host.options["proxmox-node"]);
}
function tokenEnv() {
  return str(host.options["proxmox-token-env"]) || "PVE_API_TOKEN";
}
function apiToken() {
  return host.env(tokenEnv());
}
function insecureTLS() {
  return host.options["proxmox-insecure-tls"] !== false;
}
function storage() {
  return str(host.options["proxmox-storage"]) || "local";
}
function ostemplate() {
  return str(host.options["proxmox-ostemplate"]);
}
function ostype() {
  return str(host.options["proxmox-ostype"]) || "debian";
}
function bridge() {
  return str(host.options["proxmox-bridge"]) || "vmbr0";
}
function ipAddress() {
  return str(host.options["proxmox-ip-address"]);
}
function gateway() {
  return str(host.options["proxmox-gateway"]);
}
function dnsServers() {
  return str(host.options["proxmox-dns-servers"]).split(",").map((s) => s.trim()).filter((s) => s !== "");
}
function memory() {
  return num(host.options["proxmox-memory"], 512);
}
function swap() {
  return num(host.options["proxmox-swap"], 512);
}
function cores() {
  return num(host.options["proxmox-cores"], 1);
}
function rootfsSize() {
  return num(host.options["proxmox-rootfs-size"], 8);
}
function sshKey() {
  return str(host.options["proxmox-ssh-key"]);
}
function sshPublicKeyPath() {
  const explicit = str(host.options["proxmox-ssh-public-key"]);
  return explicit !== "" ? explicit : sshKey() + ".pub";
}
function sshPort() {
  return num(host.options["proxmox-ssh-port"], 22);
}
function loginUser() {
  return str(host.options["proxmox-login-user"]) || "root";
}
function guestEscalation() {
  return str(host.options["proxmox-guest-escalation-command"]);
}
function ipBaseNetwork() {
  return str(host.options["ip-base-network"]);
}
function ipPoolSize() {
  return num(host.options["ip-pool-size"], 0);
}
function hasIPPool() {
  return ipBaseNetwork() !== "" && ipPoolSize() > 0;
}
function bootTimeoutMs() {
  return num(host.options["proxmox-boot-timeout"], 300) * 1e3;
}
function pollIntervalMs() {
  return num(host.options["proxmox-poll-interval"], 2) * 1e3;
}
function probeTimeoutSec() {
  return num(host.options["proxmox-ssh-probe-timeout"], 20);
}
function isIPv4(addr) {
  const m = ipv4RE.exec(addr);
  if (!m) return false;
  for (let i = 1; i <= 4; i++) {
    const n = parseInt(m[i], 10);
    if (n < 0 || n > 255) return false;
  }
  return true;
}
function isCIDR(addr) {
  const slash = addr.indexOf("/");
  if (slash < 0) return false;
  const ip = addr.slice(0, slash);
  const prefix = addr.slice(slash + 1);
  if (!/^\d{1,2}$/.test(prefix)) return false;
  const p = parseInt(prefix, 10);
  if (p < 0 || p > 32) return false;
  return isIPv4(ip);
}
function stripCIDR(addr) {
  const i = addr.indexOf("/");
  return i >= 0 ? addr.slice(0, i) : addr;
}
function validateWorkspaceID(id) {
  if (str(id).trim() === "") {
    throw new Error("workspace id is required");
  }
  if (id.indexOf("..") !== -1 || id.indexOf("/") !== -1 || id.indexOf("\\") !== -1 || !safeNameRE.test(id)) {
    throw new Error("invalid workspace id " + JSON.stringify(id));
  }
}
function validateHostname(name) {
  if (str(name).trim() === "") {
    throw new Error("proxmox container name is required");
  }
  if (name.indexOf(hostnamePrefix) !== 0) {
    throw new Error("proxmox container name " + JSON.stringify(name) + " must start with " + JSON.stringify(hostnamePrefix));
  }
  if (name.indexOf("..") !== -1 || name.indexOf("/") !== -1 || name.indexOf("\\") !== -1 || !safeNameRE.test(name)) {
    throw new Error("invalid proxmox container name " + JSON.stringify(name));
  }
}
function validateConfig() {
  if (apiURL() === "") {
    throw new Error("proxmox API URL is required (-proxmox-api-url)");
  }
  if (nodeName() === "") {
    throw new Error("proxmox node name is required (-proxmox-node)");
  }
  if (ostemplate() === "") {
    throw new Error("proxmox OS template is required (-proxmox-ostemplate)");
  }
  const base = ipBaseNetwork();
  const size = ipPoolSize();
  if (base === "" !== (size === 0)) {
    throw new Error("ip-base-network and ip-pool-size must be set together");
  }
  if (base !== "" && !isCIDR(base)) {
    throw new Error("invalid ip-base-network " + JSON.stringify(base));
  }
  if (size < 0) {
    throw new Error("ip-pool-size must be positive, got " + size);
  }
  if (ipAddress() !== "" && !isCIDR(ipAddress())) {
    throw new Error("proxmox-ip-address must be a CIDR address (address/prefix), got " + JSON.stringify(ipAddress()));
  }
  if (gateway() !== "" && !isIPv4(gateway())) {
    throw new Error("invalid proxmox gateway " + JSON.stringify(gateway()));
  }
  for (const server of dnsServers()) {
    if (!isIPv4(server)) {
      throw new Error("invalid proxmox DNS server IP " + JSON.stringify(server));
    }
  }
}
function apiBase() {
  return apiURL().replace(/\/+$/, "") + "/api2/json";
}
function apiArgs(method, path, form) {
  const args = ["curl", "-sS"];
  if (insecureTLS()) {
    args.push("-k");
  }
  args.push("--connect-timeout", "10", "--max-time", "30");
  if (method !== "GET") {
    args.push("-X", method);
  }
  args.push("-H", "Authorization: PVEAPIToken=" + apiToken());
  for (let i = 0; i < form.length; i++) {
    args.push("--data-urlencode", form[i][0] + "=" + form[i][1]);
  }
  args.push(apiBase() + path);
  return args;
}
function apiCall(method, path, form, quiet) {
  if (!quiet) {
    logInfo("proxmox-api " + method + " " + path);
  }
  const res = host.run(apiArgs(method, path, form));
  if (res.exit_code !== 0) {
    logWarn("proxmox-api " + method + " " + path + " failed (curl exit " + res.exit_code + "): " + str(res.stderr));
    throw new Error("proxmox API " + method + " " + path + " failed (curl exit " + res.exit_code + "): " + str(res.stderr));
  }
  const body = str(res.stdout).trim();
  let parsed;
  try {
    parsed = JSON.parse(body);
  } catch (e) {
    logWarn("proxmox-api " + method + " " + path + " returned non-JSON: " + body.slice(0, 200));
    throw new Error("proxmox API " + method + " " + path + " returned non-JSON: " + body.slice(0, 200));
  }
  if (parsed && parsed.errors !== void 0 && parsed.errors !== null) {
    logWarn("proxmox-api " + method + " " + path + " error: " + JSON.stringify(parsed.errors));
    throw new Error("proxmox API " + method + " " + path + " error: " + JSON.stringify(parsed.errors));
  }
  return parsed && parsed.data !== void 0 ? parsed.data : parsed;
}
function apiCallLenient(method, path, form) {
  try {
    return apiCall(method, path, form, true);
  } catch (e) {
    return null;
  }
}
function workspaceVMID(workspaceID) {
  let h = 2166136261;
  for (let i = 0; i < workspaceID.length; i++) {
    h ^= workspaceID.charCodeAt(i);
    h = h * 16777619 >>> 0;
  }
  return 100 + h % 999999900;
}
function vmidFromHostname(hostname) {
  return String(workspaceVMID(hostname.replace(/^wsd-/, "")));
}
function containerStatus(vmid) {
  const data = apiCallLenient("GET", "/nodes/" + nodeName() + "/lxc/" + vmid + "/status/current", []);
  return data && data.status ? str(data.status) : "";
}
function waitingWarn(lastWarn, what) {
  const now = Date.now();
  if (now - lastWarn >= 15e3) {
    logWarn("proxmox: still waiting for " + what);
    return now;
  }
  return lastWarn;
}
function waitForTask(upid) {
  const deadline = Date.now() + bootTimeoutMs();
  let lastWarn = 0;
  for (; ; ) {
    const data = apiCall("GET", "/nodes/" + nodeName() + "/tasks/" + upid + "/status", [], true);
    if (data && data.status === "stopped") {
      if (data.exitstatus && data.exitstatus !== "OK") {
        throw new Error("proxmox task " + upid + " failed: " + str(data.exitstatus));
      }
      return;
    }
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for proxmox task " + upid);
    }
    lastWarn = waitingWarn(lastWarn, "create task " + upid + " (status " + (data && data.status ? str(data.status) : "?") + ")");
    host.sleep(pollIntervalMs());
  }
}
function waitForRunning(vmid) {
  const deadline = Date.now() + bootTimeoutMs();
  let lastWarn = 0;
  for (; ; ) {
    const status = containerStatus(vmid);
    if (status === "running") {
      return;
    }
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for container " + vmid + " to become running (last status: " + status + ")");
    }
    lastWarn = waitingWarn(lastWarn, "container " + vmid + " to run (status " + status + ")");
    host.sleep(pollIntervalMs());
  }
}
function getContainerIP(vmid, fallback) {
  if (fallback !== "") {
    return fallback;
  }
  const deadline = Date.now() + bootTimeoutMs();
  let lastWarn = 0;
  for (; ; ) {
    const ifaces = apiCallLenient("GET", "/nodes/" + nodeName() + "/lxc/" + vmid + "/interfaces", []) || [];
    for (let i = 0; i < ifaces.length; i++) {
      const inet = str(ifaces[i] && ifaces[i].inet);
      if (inet !== "" && inet.indexOf("127.") !== 0 && isIPv4(inet)) {
        return inet;
      }
    }
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for container " + vmid + " to obtain an IPv4 address");
    }
    lastWarn = waitingWarn(lastWarn, "container " + vmid + " to obtain an IPv4 address");
    host.sleep(pollIntervalMs());
  }
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
  return s.indexOf("permission denied (publickey") !== -1 || s.indexOf("permission denied (keyboard-interactive") !== -1 || s.indexOf("sudo:") !== -1 || s.indexOf("doas:") !== -1 || s.indexOf("operation not permitted") !== -1;
}
function handleFor(vmid, hostname, ip, workDir) {
  const user = loginUser();
  const privilege = user === "root" ? "root" : "user";
  const escalation = user === "root" ? "" : guestEscalation();
  return {
    native_id: hostname,
    work_dir: workDir,
    username: user,
    privilege,
    os: "linux",
    vmid,
    hostname,
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
  let lastWarn = 0;
  for (; ; ) {
    if (Date.now() > deadline) {
      throw new Error("timed out waiting for TCP " + ip + ":" + port + " to open");
    }
    const res = host.run(["nc", "-z", "-w", "3", ip, String(port)]);
    if (res.exit_code === 0) {
      break;
    }
    lastWarn = waitingWarn(lastWarn, "TCP " + ip + ":" + port + " to open");
    host.sleep(2e3);
  }
  const probePayload = remotePayload(escalation, ["true"], "", ["PATH=" + guestPath]);
  lastWarn = 0;
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
    lastWarn = waitingWarn(lastWarn, "SSH on " + ip + " to accept commands");
    host.sleep(2e3);
  }
}
function destroyVM(vmid, workspaceID) {
  if (vmid === "") {
    return;
  }
  apiCallLenient("POST", "/nodes/" + nodeName() + "/lxc/" + vmid + "/status/stop", []);
  apiCallLenient("DELETE", "/nodes/" + nodeName() + "/lxc/" + vmid, [
    ["purge", "1"],
    ["destroy-unreferenced-disks", "1"]
  ]);
  if (workspaceID !== "") {
    host.ipam.release(workspaceID);
  }
}
const proxmoxLXCPlugin = {
  name: "proxmox-lxc",
  description: "Proxmox VE LXC containers, driven through the REST API and ssh(1)",
  version: "1.0.0",
  os: "linux",
  isolation: "container",
  binaries: ["curl", "ssh", "nc", "cat", "test"],
  options: [
    {
      flag: "proxmox-api-url",
      type: "string",
      default: "",
      usage: "Base URL of the Proxmox VE API, e.g. https://pve.example:8006"
    },
    {
      flag: "proxmox-node",
      type: "string",
      default: "",
      usage: "Proxmox VE node name that containers are created on"
    },
    {
      flag: "proxmox-token-env",
      type: "string",
      default: "PVE_API_TOKEN",
      usage: "Environment variable holding the Proxmox API token (PVEAPIToken=user@realm!id=uuid)"
    },
    {
      flag: "proxmox-insecure-tls",
      type: "bool",
      default: "true",
      usage: "Accept the Proxmox VE self-signed TLS certificate (curl -k)"
    },
    {
      flag: "proxmox-storage",
      type: "string",
      default: "local",
      usage: "Storage the container rootfs volume is allocated on"
    },
    {
      flag: "proxmox-ostemplate",
      type: "string",
      default: "",
      usage: "OS template or OCI image used to create containers, e.g. local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst"
    },
    {
      flag: "proxmox-ostype",
      type: "string",
      default: "debian",
      usage: "OS type used for in-container setup: debian, ubuntu, alpine, unmanaged, ..."
    },
    {
      flag: "proxmox-rootfs-size",
      type: "int",
      default: "8",
      usage: "Container rootfs size in GiB"
    },
    {
      flag: "proxmox-bridge",
      type: "string",
      default: "vmbr0",
      usage: "Bridge the container's net0 interface is attached to"
    },
    {
      flag: "proxmox-ip-address",
      type: "string",
      default: "",
      usage: "Static container IPv4 address in CIDR form, e.g. 192.168.1.50/24; empty uses DHCP"
    },
    {
      flag: "proxmox-gateway",
      type: "string",
      default: "",
      usage: "IPv4 default gateway for a static container address; empty uses the pool gateway"
    },
    {
      flag: "proxmox-dns-servers",
      type: "string",
      default: "",
      usage: "Comma-separated DNS server IPs; defaults to the host configuration"
    },
    {
      flag: "ip-base-network",
      type: "string",
      default: "",
      usage: "Base network in CIDR notation for automatic IP allocation, e.g. 192.168.1.200/24"
    },
    {
      flag: "ip-pool-size",
      type: "int",
      default: "0",
      usage: "Number of addresses to allocate from --ip-base-network"
    },
    {
      flag: "proxmox-memory",
      type: "int",
      default: "512",
      usage: "Container RAM in MB"
    },
    {
      flag: "proxmox-swap",
      type: "int",
      default: "512",
      usage: "Container swap in MB"
    },
    {
      flag: "proxmox-cores",
      type: "int",
      default: "1",
      usage: "Number of CPU cores assigned to the container"
    },
    {
      flag: "proxmox-ssh-key",
      type: "string",
      default: "/var/wsd/agent_ed25519",
      usage: "Host path to the SSH private key used to reach containers"
    },
    {
      flag: "proxmox-ssh-public-key",
      type: "string",
      default: "",
      usage: "Host path to the SSH public key injected via ssh-public-keys; empty derives <ssh-key>.pub"
    },
    {
      flag: "proxmox-ssh-port",
      type: "int",
      default: "22",
      usage: "Container SSH port"
    },
    {
      flag: "proxmox-login-user",
      type: "string",
      default: "root",
      usage: "Account the daemon SSHes in as; ssh-public-keys injects into root only"
    },
    {
      flag: "proxmox-guest-escalation-command",
      type: "string",
      default: "sudo",
      usage: "Privilege escalation command used inside the container for a non-root login user (ignored for root)"
    },
    {
      flag: "proxmox-boot-timeout",
      type: "duration",
      default: "5m",
      usage: "How long to wait for a freshly created container to accept SSH"
    },
    {
      flag: "proxmox-poll-interval",
      type: "duration",
      default: "2s",
      usage: "How often to poll task/status/IP readiness while provisioning"
    },
    {
      flag: "proxmox-ssh-probe-timeout",
      type: "duration",
      default: "20s",
      usage: "Bound on a single SSH readiness probe while waiting for a container to boot"
    }
  ],
  validate() {
    validateConfig();
    if (apiToken() === "") {
      throw new Error("proxmox API token not found in environment variable " + tokenEnv() + " (set PVEAPIToken=...)");
    }
    const key = sshKey();
    if (key === "") {
      throw new Error("proxmox SSH private key path is required (-proxmox-ssh-key)");
    }
    if (host.run(["test", "-r", key]).exit_code !== 0) {
      throw new Error("proxmox SSH private key " + key + " is not readable");
    }
    const pub = sshPublicKeyPath();
    if (pub === "") {
      throw new Error("proxmox SSH public key path is required (-proxmox-ssh-public-key)");
    }
    if (host.run(["test", "-r", pub]).exit_code !== 0) {
      throw new Error("proxmox SSH public key " + pub + " is not readable");
    }
    apiCall("GET", "/nodes/" + nodeName() + "/status", []);
  },
  provision(spec) {
    validateConfig();
    const workspaceID = str(spec.workspace_id);
    validateWorkspaceID(workspaceID);
    let ip4 = str(spec.ip4_address) !== "" ? str(spec.ip4_address) : ipAddress();
    let gw = gateway();
    if (ip4 === "" && hasIPPool()) {
      ip4 = host.ipam.allocate(workspaceID);
      if (gw === "") {
        gw = host.ipam.gateway();
      }
    }
    if (ip4 !== "" && !isCIDR(ip4)) {
      throw new Error("invalid container IPv4 address " + JSON.stringify(ip4) + " (want address/prefix)");
    }
    if (gw !== "" && !isIPv4(gw)) {
      throw new Error("invalid container gateway " + JSON.stringify(gw));
    }
    let net0 = "name=eth0,bridge=" + bridge();
    if (ip4 !== "") {
      net0 += ",ip=" + ip4;
      if (gw !== "") {
        net0 += ",gw=" + gw;
      }
    } else {
      net0 += ",ip=dhcp";
    }
    const hostname = hostnamePrefix + workspaceID;
    validateHostname(hostname);
    const pubkey = readPublicKey();
    logInfo("proxmox: provisioning " + hostname + " on node " + nodeName() + " (net0=" + net0 + ")");
    const vmid = String(workspaceVMID(workspaceID));
    logInfo("proxmox: using VMID " + vmid + " for " + hostname);
    const form = [
      ["vmid", vmid],
      ["hostname", hostname],
      ["ostemplate", ostemplate()],
      ["ostype", ostype()],
      ["storage", storage()],
      ["rootfs", storage() + ":" + String(rootfsSize())],
      ["memory", String(memory())],
      ["swap", String(swap())],
      ["cores", String(cores())],
      ["net0", net0],
      ["ssh-public-keys", pubkey],
      ["unprivileged", "1"],
      ["start", "1"]
    ];
    const dns = dnsServers();
    if (dns.length > 0) {
      form.push(["nameserver", dns[0]]);
    }
    logInfo("proxmox: creating container " + vmid + " from " + ostemplate());
    const upid = apiCall("POST", "/nodes/" + nodeName() + "/lxc", form);
    if (typeof upid !== "string" || upid === "") {
      throw new Error("proxmox create returned no task id");
    }
    logInfo("proxmox: create task " + upid);
    host.onFail(apiArgs("DELETE", "/nodes/" + nodeName() + "/lxc/" + vmid, [
      ["purge", "1"],
      ["destroy-unreferenced-disks", "1"]
    ]));
    waitForTask(upid);
    waitForRunning(vmid);
    logInfo("proxmox: container " + vmid + " is running");
    const ip = getContainerIP(vmid, ip4 === "" ? "" : stripCIDR(ip4));
    logInfo("proxmox: container " + vmid + " has address " + ip);
    waitForSSH(ip);
    logInfo("proxmox: container " + vmid + " (" + hostname + ") ready at " + ip);
    return handleFor(vmid, hostname, ip, spec.work_dir);
  },
  destroy(handle) {
    const hostname = str(handle.native_id);
    destroyVM(vmidFromHostname(hostname), hostname.replace(/^wsd-/, ""));
  },
  destroyByID(id) {
    const hostname = str(id);
    destroyVM(vmidFromHostname(hostname), hostname.replace(/^wsd-/, ""));
  },
  destroyWorkspaceByID(workspaceID) {
    validateWorkspaceID(workspaceID);
    const hostname = hostnamePrefix + workspaceID;
    destroyVM(vmidFromHostname(hostname), workspaceID);
  },
  recover(nativeID, workDir) {
    const hostname = str(nativeID);
    validateHostname(hostname);
    const vmid = vmidFromHostname(hostname);
    const status = containerStatus(vmid);
    if (status === "") {
      throw new Error("container " + hostname + " not found");
    }
    if (status !== "running") {
      apiCall("POST", "/nodes/" + nodeName() + "/lxc/" + vmid + "/status/start", []);
      waitForRunning(vmid);
    }
    const ip = getContainerIP(vmid, "");
    if (hasIPPool() && ip !== "") {
      host.ipam.reserve(hostname.replace(/^wsd-/, ""), ip);
    }
    return handleFor(vmid, hostname, ip, workDir);
  },
  cleanupOrphans(keep) {
    const keepSet = {};
    for (const name of keep) {
      keepSet[name] = true;
    }
    let removed = 0;
    const list = apiCallLenient("GET", "/nodes/" + nodeName() + "/lxc", []) || [];
    for (let i = 0; i < list.length; i++) {
      const entry = list[i];
      const hostname = str(entry && entry.name);
      if (hostname.indexOf(hostnamePrefix) !== 0 || keepSet[hostname]) {
        continue;
      }
      const vmid = str(entry && entry.vmid);
      try {
        destroyVM(vmid, hostname.replace(/^wsd-/, ""));
        removed++;
      } catch (e) {
        host.log(1, "cleanupOrphans failed for " + hostname + ": " + str(e));
      }
    }
    return removed;
  },
  execArgs(handle, cmd, workDir, env, _opts) {
    const fullEnv = ["PATH=" + guestPath].concat(env || []);
    const payload = remotePayload(str(handle.escalation), cmd, workDir, fullEnv);
    return sshArgv(str(handle.ip), num(handle.port, sshPort()), str(handle.username), str(handle.key_path), payload, 55);
  },
  bootstrapCmd() {
    const script = "apt-get update && apt-get install -y --no-install-recommends git ca-certificates && git config --system --add safe.directory '*'";
    return ["sh", "-c", script];
  }
};
function readPublicKey() {
  const res = host.run(["cat", sshPublicKeyPath()]);
  if (res.exit_code !== 0) {
    throw new Error("failed to read SSH public key " + sshPublicKeyPath() + ": " + res.stderr);
  }
  return str(res.stdout).trim();
}
module.exports = proxmoxLXCPlugin;
