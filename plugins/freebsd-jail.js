"use strict";
function str(v) {
  return typeof v === "string" ? v : v === void 0 || v === null ? "" : String(v);
}
function num(v, def) {
  return typeof v === "number" ? v : def;
}
function boolVal(v) {
  return v === true;
}
const jailConfPath = "/etc/jail.conf.d";
const baseJailPath = "/usr/local/jails";
const containersDirName = "containers";
const defaultRelease = "15.1-RELEASE";
const defaultBaseURL = "https://download.freebsd.org/releases";
const jailPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin";
const bootstrapScript = "certctl rehash && env ASSUME_ALWAYS_YES=yes pkg bootstrap -y && pkg install -y git ca_root_nss && git config --system --add safe.directory '*'";
const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const ipv4RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const jailIP4AddrRE = /ip4\.addr \+= "([^"]+)"/;
const jailIfconfigIPRE = /\/sbin\/ifconfig \S+ (\S+) up/;
function release() {
  return str(host.options["freebsd-release"]) || defaultRelease;
}
function arch() {
  return str(host.options["freebsd-arch"]) || "amd64";
}
function networkInterface() {
  return str(host.options["freebsd-interface"]);
}
function ip4Address() {
  return str(host.options["freebsd-ip4-address"]);
}
function networkMode() {
  return str(host.options["freebsd-network-mode"]) || "shared";
}
function vnetGateway() {
  return str(host.options["freebsd-vnet-gateway"]);
}
function ipPoolSize() {
  return num(host.options["ip-pool-size"], 0);
}
function hasIPPool() {
  return str(host.options["ip-base-network"]) !== "" && ipPoolSize() > 0;
}
function dnsServers() {
  return str(host.options["freebsd-dns-servers"]).split(",").map((s) => s.trim()).filter((s) => s !== "");
}
function escalator() {
  return str(host.options["escalation-command"]);
}
function templateBaseURL() {
  return str(host.options["base-template-url"]);
}
function templateUser() {
  return str(host.options["base-template-user"]);
}
function templatePasswordEnv() {
  return str(host.options["base-template-password-env"]);
}
function templateHeader() {
  return str(host.options["base-template-header"]);
}
function templateHeaderEnv() {
  return str(host.options["base-template-header-env"]);
}
function verifySignature() {
  return boolVal(host.options["verify-signature"]);
}
function templatePublicKey() {
  return str(host.options["template-public-key"]);
}
function escalate(argv) {
  const esc = escalator();
  return esc ? [esc].concat(argv) : argv;
}
function jailRootPath(jailName) {
  return baseJailPath + "/" + containersDirName + "/" + jailName;
}
function jailConfigPath(jailName) {
  return jailConfPath + "/" + jailName + ".conf";
}
function baseArchivePath(releaseName) {
  return baseJailPath + "/media/" + releaseName + "/base.txz";
}
function freebsdMachine(a) {
  return a === "arm64" ? "aarch64" : a;
}
function baseArchiveURL(releaseName) {
  return defaultBaseURL + "/" + arch() + "/" + freebsdMachine(arch()) + "/" + releaseName + "/base.txz";
}
function stripLeading(p) {
  let out = p;
  while (out.indexOf("/") === 0) out = out.slice(1);
  return out;
}
function validateWorkspaceID(id) {
  if (str(id).trim() === "") {
    throw new Error("workspace id is required");
  }
  if (id.indexOf("..") !== -1 || id.indexOf("/") !== -1 || id.indexOf("\\") !== -1 || !safeNameRE.test(id)) {
    throw new Error("invalid workspace id " + JSON.stringify(id));
  }
}
function validateReleaseName(name) {
  if (name === "") {
    throw new Error("freebsd release is required");
  }
  if (name.indexOf("..") !== -1 || /[\/\\]/.test(name) || !safeNameRE.test(name)) {
    throw new Error("invalid freebsd release " + JSON.stringify(name));
  }
}
function validateInterfaceName(name) {
  if (name === "") {
    throw new Error("freebsd jail interface is required (set interface in the prepare request or -freebsd-interface)");
  }
  if (name.indexOf("..") !== -1 || /[\/\\ ]/.test(name) || !safeNameRE.test(name)) {
    throw new Error("invalid freebsd jail interface " + JSON.stringify(name));
  }
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
function validateIP4Address(addr) {
  if (addr === "") {
    throw new Error("freebsd jail ip4 address is required (set ip4_address in the prepare request or -freebsd-ip4-address)");
  }
  if (!isIPv4(addr)) {
    throw new Error("invalid freebsd jail IPv4 address " + JSON.stringify(addr));
  }
}
function validateVNETIP4Address(addr) {
  const slash = addr.indexOf("/");
  const ipPart = slash >= 0 ? addr.slice(0, slash) : addr;
  const prefix = slash >= 0 ? addr.slice(slash + 1) : "";
  if (slash < 0 || !isIPv4(ipPart) || !/^\d{1,2}$/.test(prefix)) {
    throw new Error("invalid freebsd VNET IPv4 address " + JSON.stringify(addr) + " (want address/prefix)");
  }
}
function validateJailRoot(path) {
  if (str(path).trim() === "") {
    throw new Error("freebsd jail root path is empty");
  }
  if (path.indexOf("/") !== 0) {
    throw new Error("freebsd jail root path " + JSON.stringify(path) + " must be absolute");
  }
  if (path === "/") {
    throw new Error("refusing to use / as a freebsd jail root");
  }
  if (path === baseJailPath) {
    throw new Error("refusing to use base jail path " + JSON.stringify(path) + " as a jail root");
  }
}
function validateDNS() {
  for (const server of dnsServers()) {
    if (!isIPv4(server)) {
      throw new Error("invalid freebsd DNS server IP " + JSON.stringify(server));
    }
  }
  const mode = networkMode();
  if (mode === "vnet" && (ip4Address() !== "" || vnetGateway() !== "" || ipPoolSize() > 0) && dnsServers().length === 0) {
    throw new Error("static freebsd VNET requires explicit DNS servers");
  }
}
function isValidHeaderName(h) {
  return /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(h);
}
function validateTemplate() {
  const baseURL = templateBaseURL();
  const username = templateUser();
  const passwordEnv = templatePasswordEnv();
  const header = templateHeader();
  const headerEnv = templateHeaderEnv();
  const verify = verifySignature();
  const publicKey = templatePublicKey();
  if (baseURL === "") {
    if (username !== "" || passwordEnv !== "" || header !== "" || headerEnv !== "" || verify || publicKey !== "") {
      throw new Error("template authentication and signature options require base-template-url");
    }
    return;
  }
  const schemeEnd = baseURL.indexOf("://");
  if (baseURL.indexOf("http://") !== 0 && baseURL.indexOf("https://") !== 0 || schemeEnd < 0) {
    throw new Error("base-template-url must be an HTTP(S) directory URL without credentials, query, or fragment");
  }
  const rest = baseURL.slice(schemeEnd + 3);
  const slash = rest.indexOf("/");
  const authority = slash >= 0 ? rest.slice(0, slash) : rest;
  if (authority === "" || authority.indexOf("@") !== -1 || baseURL.indexOf("?") !== -1 || baseURL.indexOf("#") !== -1) {
    throw new Error("base-template-url must be an HTTP(S) directory URL without credentials, query, or fragment");
  }
  validateReleaseName(release());
  if (release() === "." || release() === "..") {
    throw new Error("template release must be a filename");
  }
  if (username === "" !== (passwordEnv === "")) {
    throw new Error("template Basic authentication requires both user and password-env");
  }
  if (header === "" !== (headerEnv === "")) {
    throw new Error("template header authentication requires both header and header-env");
  }
  if (username !== "" && header !== "") {
    throw new Error("choose template Basic authentication or header authentication, not both");
  }
  if (header !== "") {
    if (!isValidHeaderName(header)) {
      throw new Error("invalid template HTTP header name");
    }
    switch (header.toLowerCase()) {
      case "host":
      case "content-length":
      case "transfer-encoding":
      case "connection":
        throw new Error("template authentication header cannot control HTTP routing or framing");
    }
  }
  for (const name of [passwordEnv, headerEnv]) {
    if (name === "") continue;
    const value = host.env(name);
    if (value === "" || value.indexOf("\r") !== -1 || value.indexOf("\n") !== -1) {
      throw new Error("template credential environment variable " + name + " is missing, empty, or contains a newline");
    }
  }
  if (verify) {
    if (publicKey === "") {
      throw new Error("verify-signature requires template-public-key");
    }
    if (host.run(["test", "-r", publicKey]).exit_code !== 0) {
      throw new Error("template public key " + publicKey + " is not readable");
    }
  } else if (publicKey !== "") {
    throw new Error("template-public-key requires verify-signature");
  }
}
function fnv1a32(s, seed) {
  let h = seed >>> 0;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = h * 16777619 >>> 0;
  }
  return h;
}
function stableDigest40(s) {
  const a = fnv1a32(s, 2166136261);
  const b = fnv1a32(s, 3735928559);
  const hex = ("00000000" + a.toString(16)).slice(-8) + ("00000000" + b.toString(16)).slice(-8);
  return hex.slice(0, 10);
}
function jibNameForJail(jailName) {
  let name = jailName;
  if (name.indexOf("wsd-") === 0) name = name.slice(4);
  name = name.replace(/-/g, "");
  if (name.length <= 11) return name;
  return "w" + stableDigest40(jailName);
}
function pathExists(p) {
  return host.run(["test", "-e", p]).exit_code === 0;
}
function makeJailDirectories(jailRoot, paths) {
  if (escalator() !== "") {
    const res2 = host.run(escalate(["chroot", jailRoot, "/bin/mkdir", "-p", "--"].concat(paths)));
    if (res2.exit_code !== 0) {
      throw new Error("failed to create jail directories: " + res2.stderr);
    }
    return;
  }
  const res = host.run(["mkdir", "-p"].concat(paths.map((p) => jailRoot + "/" + p)));
  if (res.exit_code !== 0) {
    throw new Error("failed to create jail directories: " + res.stderr);
  }
}
function ensurePkgDBOwnership(jailRoot) {
  const pkgdb = jailRoot + "/var/db/pkg";
  if (host.run(escalate(["mkdir", "-p", pkgdb])).exit_code !== 0) {
    host.log(1, "could not create pkg database dir " + pkgdb);
    return;
  }
  if (host.run(escalate(["chown", "0:0", pkgdb])).exit_code !== 0) {
    host.log(1, "could not chown pkg database dir " + pkgdb + " to root");
  }
}
function copyHostResolvConf(jailRoot) {
  const servers = dnsServers();
  let content = "";
  if (servers.length > 0) {
    for (const server of servers) {
      content += "nameserver " + server + "\n";
    }
  } else {
    const res = host.run(["cat", "/etc/resolv.conf"]);
    if (res.exit_code !== 0) {
      throw new Error("failed to read host /etc/resolv.conf: " + res.stderr);
    }
    content = res.stdout;
  }
  if (escalator() !== "") {
    const res = host.run(
      escalate(["chroot", jailRoot, "/bin/sh", "-c", `/bin/mkdir -p /etc && umask 022 && printf '%s' "$1" > /etc/resolv.conf`, "mcwsd-dns", content])
    );
    if (res.exit_code !== 0) {
      throw new Error("failed to install jail DNS configuration: " + res.stderr);
    }
    return;
  }
  if (host.run(["mkdir", "-p", jailRoot + "/etc"]).exit_code !== 0) {
    throw new Error("failed to create jail /etc: ");
  }
  if (host.run(["tee", jailRoot + "/etc/resolv.conf"], { stdin: content }).exit_code !== 0) {
    throw new Error("failed to write jail resolv.conf");
  }
}
function unmountJailRoot(jailRoot) {
  const mounts = [
    jailRoot + "/workspace/repo",
    jailRoot + "/cache",
    jailRoot + "/tmp",
    jailRoot + "/dev"
  ];
  host.run(["mount"]);
  for (const mp of mounts) {
    if (!pathExists(mp)) continue;
    host.run(escalate(["umount", "-f", mp]));
  }
}
function removeJailRoot(jailRoot) {
  host.run(escalate(["rm", "-rf", jailRoot]));
}
function ensureSafeJailRoot(jailRoot) {
  const ls = host.run(["ls", "-A", jailRoot]);
  if (ls.exit_code !== 0 || str(ls.stdout).trim() === "") {
    return;
  }
  if (host.run(["test", "-e", jailRoot + "/bin/sh"]).exit_code === 0) {
    return;
  }
  throw new Error("refusing to extract into non-empty directory " + jailRoot);
}
function ensureBaseArchive(releaseName) {
  if (templateBaseURL() !== "") {
    return ensureTemplateArchive(releaseName);
  }
  const dest = baseArchivePath(releaseName);
  if (host.run(["test", "-s", dest]).exit_code === 0) {
    return dest;
  }
  if (host.run(["mkdir", "-p", dest.slice(0, dest.lastIndexOf("/"))]).exit_code !== 0) {
    throw new Error("failed to create base archive cache directory");
  }
  const url = baseArchiveURL(releaseName);
  const res = host.run(["fetch", "-o", dest, url]);
  if (res.exit_code !== 0) {
    host.run(["rm", "-f", dest]);
    throw new Error("failed to download base archive for release " + releaseName + " from " + url + ": " + res.stderr);
  }
  return dest;
}
function templateCachePath(filename) {
  const source = templateBaseURL() + "/" + filename;
  return baseJailPath + "/media/templates/" + stableDigest40(source) + "/" + filename;
}
function ensureTemplateArchive(filename) {
  validateTemplate();
  if (filename.indexOf("..") !== -1 || /[\/\\]/.test(filename) || !safeNameRE.test(filename) || filename === "." || filename === "..") {
    throw new Error("template release must be a safe filename");
  }
  const dest = templateCachePath(filename);
  if (host.run(["test", "-s", dest]).exit_code === 0) {
    return dest;
  }
  const url = templateBaseURL() + "/" + filename;
  if (host.run(["mkdir", "-p", dest.slice(0, dest.lastIndexOf("/"))]).exit_code !== 0) {
    throw new Error("failed to create template cache directory");
  }
  const opts = {};
  if (templateUser() !== "") {
    opts.username = templateUser();
    opts.password_env = templatePasswordEnv();
  }
  if (templateHeader() !== "") {
    opts.header = templateHeader();
    opts.header_env = templateHeaderEnv();
  }
  if (verifySignature()) {
    opts.public_key = templatePublicKey();
  }
  const scratch = host.fetch(url, opts);
  if (host.run(["cp", scratch, dest]).exit_code !== 0) {
    throw new Error("failed to cache template archive");
  }
  return dest;
}
function populateJailRoot(jailRoot, baseArchive) {
  if (host.run(["mkdir", "-p", jailRoot]).exit_code !== 0) {
    throw new Error("failed to create jail root " + jailRoot);
  }
  unmountJailRoot(jailRoot);
  try {
    ensureSafeJailRoot(jailRoot);
  } catch (e) {
    removeJailRoot(jailRoot);
    if (host.run(["mkdir", "-p", jailRoot]).exit_code !== 0) {
      throw new Error("failed to recreate jail root " + jailRoot);
    }
  }
  const extract = host.run(escalate(["tar", "-xpf", baseArchive, "-C", jailRoot]));
  if (extract.exit_code !== 0) {
    throw new Error("failed to extract base archive into " + jailRoot + ": " + extract.stderr);
  }
  if (templateBaseURL() !== "" && host.run(["test", "-e", jailRoot + "/bin/sh"]).exit_code !== 0) {
    throw new Error("template must contain a FreeBSD root with bin/sh at the archive root");
  }
  ensurePkgDBOwnership(jailRoot);
  if (host.run(["mkdir", "-p", jailRoot + "/dev"]).exit_code !== 0) {
    throw new Error("failed to create /dev in jail root");
  }
  try {
    copyHostResolvConf(jailRoot);
  } catch (e) {
    if (templateBaseURL() !== "" || dnsServers().length > 0) {
      throw e;
    }
    host.log(1, "could not install host resolv.conf in jail root " + jailRoot);
  }
}
function ensureJailMountPoints(jailRoot, workDir, cacheMountPath, workspaceHostDir) {
  makeJailDirectories(jailRoot, ["tmp"]);
  if (escalator() !== "") {
    const chmod = host.run(escalate(["chroot", jailRoot, "/bin/chmod", "1777", "/tmp"]));
    if (chmod.exit_code !== 0) {
      throw new Error("failed to set jail /tmp permissions: " + chmod.stderr);
    }
  } else {
    if (host.run(["chmod", "1777", jailRoot + "/tmp"]).exit_code !== 0) {
      throw new Error("failed to set jail /tmp permissions");
    }
  }
  if (workspaceHostDir !== "" && workDir !== "") {
    if (host.run(["mkdir", "-p", workspaceHostDir]).exit_code !== 0) {
      throw new Error("failed to create workspace host dir " + workspaceHostDir);
    }
    makeJailDirectories(jailRoot, [stripLeading(workDir)]);
  }
  if (cacheMountPath !== "") {
    if (!pathExists(cacheMountPath)) {
      host.log(1, "WARNING: cache mount path " + cacheMountPath + " does not exist on the host; jail mount and startup will fail");
    }
    makeJailDirectories(jailRoot, ["cache"]);
  }
}
function buildJailConfig(jailName, jailRoot, mode, iface, ip4, gateway, workDir, cacheMountPath, workspaceHostDir) {
  const lines = [];
  lines.push(jailName + " {");
  lines.push('  path = "' + jailRoot + '";');
  lines.push('  host.hostname = "jail-' + jailName + '.local";');
  if (mode === "vnet") {
    const jibName = jibNameForJail(jailName);
    const vnetInterface = "e0b_" + jibName;
    lines.push("  vnet;");
    lines.push('  vnet.interface = "' + vnetInterface + '";');
    lines.push("  devfs_ruleset = 11;");
    lines.push('  exec.prestart = "/usr/local/sbin/jib addm ' + jibName + " " + iface + '";');
    if (ip4 !== "") {
      lines.push('  exec.start = "/sbin/ifconfig ' + vnetInterface + " " + ip4 + ' up";');
      lines.push('  exec.start += "/sbin/route add default ' + gateway + '";');
      lines.push('  exec.start += "/bin/sh /etc/rc";');
    } else {
      lines.push('  exec.start = "/sbin/dhclient ' + vnetInterface + '";');
      lines.push('  exec.start += "/bin/sh /etc/rc";');
    }
    lines.push('  exec.poststop = "/usr/local/sbin/jib destroy ' + jibName + '";');
  } else if (ip4 !== "") {
    lines.push('  interface = "' + iface + '";');
    lines.push('  ip4.addr += "' + ip4 + '";');
  } else {
    lines.push("  ip4 = inherit;");
  }
  lines.push("  mount.devfs;");
  lines.push('  mount += "/tmp ' + jailRoot + '/tmp nullfs rw 0 0";');
  if (cacheMountPath !== "") {
    lines.push('  mount += "' + cacheMountPath + " " + jailRoot + '/cache nullfs rw 0 0";');
  }
  if (workspaceHostDir !== "") {
    const target = jailRoot + "/" + stripLeading(workDir);
    lines.push('  mount += "' + workspaceHostDir + " " + target + ' nullfs rw 0 0";');
  }
  lines.push("  exec.clean;");
  if (mode !== "vnet") {
    lines.push('  exec.start = "/bin/sh /etc/rc";');
  }
  lines.push('  exec.stop = "/bin/sh /etc/rc.shutdown";');
  lines.push("  allow.raw_sockets = 1;");
  lines.push("  allow.sysvipc;");
  lines.push("}");
  return lines.join("\n");
}
function writeJailConfig(jailName, content) {
  if (host.run(["tee", jailConfigPath(jailName)], { stdin: content }).exit_code !== 0) {
    throw new Error("failed to write jail config");
  }
}
function isBenignJailError(stderr) {
  const s = str(stderr).toLowerCase();
  if (s.indexOf("executable file not found") !== -1) {
    return false;
  }
  return s.indexOf("not found") !== -1 || s.indexOf("no such jail") !== -1 || s.indexOf("not running") !== -1 || s.indexOf("already stopped") !== -1;
}
function destroyJail(jailName) {
  validateJailName(jailName);
  const jailRoot = jailRootPath(jailName);
  const stop = host.run(escalate(["jail", "-r", jailName]));
  if (stop.exit_code !== 0 && !isBenignJailError(stop.stderr)) {
    throw new Error("failed to stop jail " + jailName + ": " + stop.stderr);
  }
  host.run(["rm", "-f", jailConfigPath(jailName)]);
  unmountJailRoot(jailRoot);
  removeJailRoot(jailRoot);
  host.ipam.release(jailName.replace(/^wsd-/, ""));
}
function validateJailName(jailName) {
  if (str(jailName).trim() === "") {
    throw new Error("jail name is required");
  }
  if (jailName.indexOf("wsd-") !== 0) {
    throw new Error("jail name " + JSON.stringify(jailName) + ' must start with "wsd-"');
  }
  if (jailName.indexOf("..") !== -1 || jailName.indexOf("/") !== -1 || jailName.indexOf("\\") !== -1 || !safeNameRE.test(jailName)) {
    throw new Error("invalid jail name " + JSON.stringify(jailName));
  }
}
function extractJailIPv4(config) {
  let m = jailIP4AddrRE.exec(config);
  if (m) return stripCIDR(m[1]);
  m = jailIfconfigIPRE.exec(config);
  if (m) return stripCIDR(m[1]);
  return "";
}
function stripCIDR(addr) {
  const i = addr.indexOf("/");
  return i >= 0 ? addr.slice(0, i) : addr;
}
function handleFor(jailName, workDir, vnetInterface) {
  return {
    native_id: jailName,
    work_dir: workDir,
    username: "root",
    privilege: "root",
    os: "freebsd",
    vnet_interface: vnetInterface
  };
}
const freebsdJailPlugin = {
  name: "freebsd-jail",
  description: "FreeBSD jails, driven through jail(8)/jexec(8)",
  version: "1.0.0",
  os: "freebsd",
  isolation: "jail",
  binaries: [
    "jail",
    "jexec",
    "jls",
    "tar",
    "chroot",
    "umount",
    "mount",
    "rm",
    "mkdir",
    "chmod",
    "chown",
    "cat",
    "test",
    "ls",
    "tee",
    "cp",
    "fetch",
    "mdo",
    "sudo",
    "doas"
  ],
  options: [
    { flag: "freebsd-release", type: "string", default: "15.1-RELEASE", usage: "FreeBSD release, or archive filename when base-template-url is configured" },
    { flag: "freebsd-arch", type: "string", default: "amd64", usage: "FreeBSD architecture used to construct the base.txz download URL (amd64, arm64, or i386)" },
    { flag: "freebsd-interface", type: "string", default: "", usage: "Host network interface for FreeBSD jails, e.g. vtnet0 (only needed with a fixed IP; may be supplied per workspace)" },
    { flag: "freebsd-ip4-address", type: "string", default: "", usage: "IPv4 address for FreeBSD jails: plain address in shared mode, CIDR address in VNET mode (optional, omit for DHCP)" },
    { flag: "freebsd-network-mode", type: "string", default: "shared", usage: "FreeBSD jail network mode: shared or vnet" },
    { flag: "freebsd-vnet-gateway", type: "string", default: "", usage: "IPv4 default gateway for VNET FreeBSD jails; required only with static IP in VNET mode" },
    { flag: "ip-base-network", type: "string", default: "", usage: "Base network in CIDR notation for automatic IP allocation, e.g. 192.168.1.200/24" },
    { flag: "ip-pool-size", type: "int", default: "0", usage: "Number of IP addresses to allocate from the base network; used with ip-base-network" },
    { flag: "freebsd-dns-servers", type: "string", default: "", usage: "Comma-separated DNS server IPs; overrides template/host resolv.conf; required for static VNET" },
    { flag: "escalation-command", type: "string", default: "mdo", usage: "Host privilege escalation command for jail(8)/jexec(8)/tar, e.g. mdo, sudo, or doas (empty runs directly)" },
    { flag: "base-template-url", type: "string", default: "", usage: "HTTP(S) directory URL containing custom jail archives; freebsd-release selects the filename" },
    { flag: "base-template-user", type: "string", default: "", usage: "HTTP Basic authentication username for jail template downloads" },
    { flag: "base-template-password-env", type: "string", default: "", usage: "Environment variable containing the template HTTP Basic authentication password" },
    { flag: "base-template-header", type: "string", default: "", usage: "Authentication header name for jail template downloads, e.g. Authorization" },
    { flag: "base-template-header-env", type: "string", default: "", usage: "Environment variable containing the complete template authentication header value" },
    { flag: "verify-signature", type: "bool", default: "false", usage: "Require a valid detached OpenPGP signature at <archive>.sig before using a jail template" },
    { flag: "template-public-key", type: "string", default: "", usage: "Local trusted OpenPGP public-key file for verify-signature (armored or binary)" }
  ],
  init() {
    validateTemplate();
    validateDNS();
    if (escalator() !== "") {
      const res = host.run(escalate(["jls"]));
      if (res.exit_code !== 0) {
        throw new Error("privilege escalator " + escalator() + " is unavailable: " + res.stderr);
      }
    }
  },
  provision(spec) {
    const workspaceID = str(spec.workspace_id);
    validateWorkspaceID(workspaceID);
    let iface = networkInterface();
    let ip4 = ip4Address();
    let mode = networkMode();
    let gateway = vnetGateway();
    const releaseName = release();
    if (str(spec.interface) !== "") {
      iface = str(spec.interface);
    }
    if (str(spec.ip4_address) !== "") {
      ip4 = str(spec.ip4_address);
    }
    if (ip4 === "" && hasIPPool()) {
      const assigned = host.ipam.allocate(workspaceID);
      ip4 = assigned;
      if (gateway === "" && mode === "vnet") {
        gateway = host.ipam.gateway();
      }
    }
    validateReleaseName(releaseName);
    if (mode === "") {
      mode = "shared";
    }
    if (mode !== "shared" && mode !== "vnet") {
      throw new Error("unsupported freebsd jail network mode " + JSON.stringify(mode) + " (want shared or vnet)");
    }
    if (mode === "vnet") {
      if (ip4 !== "") {
        validateVNETIP4Address(ip4);
        validateIP4Address(gateway);
        if (dnsServers().length === 0) {
          throw new Error("static freebsd VNET requires explicit DNS servers");
        }
      } else if (gateway !== "") {
        throw new Error("freebsd VNET gateway provided without an IPv4 address; provide a CIDR address or omit both for DHCP");
      }
    }
    if (mode === "vnet") {
      validateInterfaceName(iface);
    } else if (ip4 === "") {
    } else {
      validateInterfaceName(iface);
      validateIP4Address(ip4);
    }
    const jailName = "wsd-" + workspaceID;
    const jibName = jibNameForJail(jailName);
    const vnetInterface = "e0b_" + jibName;
    const jailRoot = jailRootPath(jailName);
    validateJailRoot(jailRoot);
    let workspaceHostDir = "";
    if (str(spec.workspace_base_path) !== "") {
      workspaceHostDir = str(spec.workspace_base_path) + "/" + workspaceID;
    }
    const rootExisted = pathExists(jailRoot);
    const baseArchive = ensureBaseArchive(releaseName);
    populateJailRoot(jailRoot, baseArchive);
    if (!rootExisted) {
      host.onFail(escalate(["rm", "-rf", jailRoot]));
    }
    ensureJailMountPoints(jailRoot, str(spec.work_dir), str(spec.cache_mount_path), workspaceHostDir);
    const config = buildJailConfig(jailName, jailRoot, mode, iface, ip4, gateway, str(spec.work_dir), str(spec.cache_mount_path), workspaceHostDir);
    writeJailConfig(jailName, config);
    host.onFail(["rm", "-f", jailConfigPath(jailName)]);
    host.onFail(escalate(["jail", "-r", jailName]));
    const start = host.run(escalate(["jail", "-c", "-f", jailConfigPath(jailName)]));
    if (start.exit_code !== 0) {
      throw new Error("failed to start jail: " + start.stderr);
    }
    return handleFor(jailName, str(spec.work_dir), vnetInterface);
  },
  destroy(handle) {
    destroyJail(str(handle.native_id));
  },
  destroyByID(id) {
    destroyJail(id);
  },
  destroyWorkspaceByID(workspaceID) {
    validateWorkspaceID(workspaceID);
    destroyJail("wsd-" + workspaceID);
  },
  recover(nativeID, workDir) {
    validateJailName(nativeID);
    const res = host.run(["cat", jailConfigPath(nativeID)]);
    if (res.exit_code !== 0) {
      throw new Error("jail " + nativeID + " not found");
    }
    const config = res.stdout;
    if (hasIPPool()) {
      const ip = extractJailIPv4(config);
      if (ip !== "") {
        host.ipam.reserve(nativeID.replace(/^wsd-/, ""), ip);
      }
    }
    return handleFor(nativeID, workDir, "e0b_" + jibNameForJail(nativeID));
  },
  cleanupOrphans(keep) {
    const keepSet = {};
    for (const name of keep) {
      keepSet[name] = true;
    }
    let removed = 0;
    const ls = host.run(["ls", "-1", baseJailPath + "/" + containersDirName]);
    if (ls.exit_code !== 0) {
      return 0;
    }
    for (const raw of str(ls.stdout).split("\n")) {
      const name = raw.trim();
      if (name.indexOf("wsd-") !== 0 || keepSet[name]) {
        continue;
      }
      try {
        destroyJail(name);
        removed++;
      } catch (e) {
        host.log(1, "cleanupOrphans failed for " + name + ": " + str(e));
      }
    }
    return removed;
  },
  execArgs(handle, cmd, workDir, env, _opts) {
    const args = ["jexec"];
    if (workDir !== "") {
      args.push("-d", workDir);
    }
    args.push(str(handle.native_id), "env", "PATH=" + jailPath);
    for (const e of env || []) {
      args.push(e);
    }
    return escalate(args.concat(cmd));
  },
  setNetwork(handle, enabled) {
    if (networkMode() === "vnet") {
      const vif = str(handle.vnet_interface);
      if (vif === "") {
        throw new Error("VNET jail " + str(handle.native_id) + " has no network interface");
      }
      const state = enabled ? "up" : "down";
      const res2 = host.run(escalate(["jexec", str(handle.native_id), "ifconfig", vif, state]));
      if (res2.exit_code !== 0) {
        throw new Error("failed to set VNET jail network " + state + ": " + res2.stderr);
      }
      return;
    }
    const mode = enabled ? "inherit" : "disable";
    const res = host.run(escalate(["jail", "-m", "name=" + str(handle.native_id), "ip4=" + mode]));
    if (res.exit_code !== 0) {
      throw new Error("failed to set jail network " + mode + ": " + res.stderr);
    }
  },
  bootstrapCmd() {
    return ["sh", "-c", bootstrapScript];
  }
};
module.exports = freebsdJailPlugin;
