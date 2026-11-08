/// <reference path="../../types/mcwsd-runtime.d.ts" />
import type { RuntimePlugin, ProvisionSpec, Handle, ExecArgsOptions } from "../../types/mcwsd-runtime";
"use strict";

/**
 * illumos / OpenIndiana zones runtime plugin: provisions each workspace as a
 * zone driven entirely through zonecfg(8), zoneadm(8), zlogin(1), and
 * dladm(8). This is the port of the former native zones.ZonesProvisioner and
 * zones.ZonesExecTransport.
 *
 * A workspace gets its own zone named wsd-<workspace-id>, either installed from
 * the ipkg brand or cloned from a golden zone, then booted and reached through
 * zlogin. Shared-IP zones share the global zone's stack; exclusive-IP zones get
 * a dedicated VNIC and run DHCP or static addressing inside the zone.
 *
 * Options (the former --zones-* flags):
 *   --zones-zonepath / --zones-brand / --zones-base-zone
 *   --zones-network-mode / --zones-interface / --zones-ip-address / --zones-defrouter
 *   --ip-base-network / --ip-pool-size   reserved: shared IP pool (core-owned)
 *   --zones-dns-servers
 *   --zones-escalation                  host escalator (pfexec/sudo/doas, "" = direct)
 *   --zones-boot-timeout / --zones-poll-interval  readiness bounds
 */

function str(v: unknown): string {
  return typeof v === "string" ? v : v === undefined || v === null ? "" : String(v);
}

function num(v: unknown, def: number): number {
  return typeof v === "number" ? v : def;
}

// --- constants (mirroring the native provisioner/transport) ---

const zonesPath = "/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin:/usr/local/bin";
const bootstrapScript = "pkg install git system/ca-certificates && git config --system --add safe.directory '*'";
const zoneNamePrefix = "wsd-";
const defaultBrand = "ipkg";
const defaultBrandDir = "/usr/lib/brand";

const safeNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const ipv4RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

// --- options ---

function zonepath(): string {
  return str(host.options["zones-zonepath"]);
}
function brand(): string {
  return str(host.options["zones-brand"]) || defaultBrand;
}
function baseZone(): string {
  return str(host.options["zones-base-zone"]);
}
function networkMode(): string {
  return str(host.options["zones-network-mode"]) || "shared";
}
function iface(): string {
  return str(host.options["zones-interface"]);
}
function ipAddress(): string {
  return str(host.options["zones-ip-address"]);
}
function defRouter(): string {
  return str(host.options["zones-defrouter"]);
}
function ipBaseNetwork(): string {
  return str(host.options["ip-base-network"]);
}
function ipPoolSize(): number {
  return num(host.options["ip-pool-size"], 0);
}
function hasIPPool(): boolean {
  return ipBaseNetwork() !== "" && ipPoolSize() > 0;
}
function dnsServers(): string[] {
  return str(host.options["zones-dns-servers"])
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}
function escalationCommand(): string {
  return str(host.options["zones-escalation"]);
}
// Durations arrive as seconds (float64) from the option binder.
function bootTimeoutMs(): number {
  return num(host.options["zones-boot-timeout"], 300) * 1000;
}
function pollIntervalMs(): number {
  return num(host.options["zones-poll-interval"], 2) * 1000;
}

// escalate prepends the host escalator (pfexec/sudo/doas) to a host argv.
function escalate(argv: string[]): string[] {
  const esc = escalationCommand();
  return esc ? [esc].concat(argv) : argv;
}

function zonePathFor(zoneName: string): string {
  return zonepath() + "/" + zoneName;
}

// --- IP / address validation (goja has no net.ParseIP) ---

function isIPv4(addr: string): boolean {
  const m = ipv4RE.exec(addr);
  if (!m) return false;
  for (let i = 1; i <= 4; i++) {
    const n = parseInt(m[i], 10);
    if (n < 0 || n > 255) return false;
  }
  return true;
}

function isCIDRIPv4(addr: string): boolean {
  const slash = addr.indexOf("/");
  if (slash < 0) return false;
  const ip = addr.slice(0, slash);
  const prefix = addr.slice(slash + 1);
  if (!/^\d{1,2}$/.test(prefix)) return false;
  const p = parseInt(prefix, 10);
  if (p < 0 || p > 32) return false;
  return isIPv4(ip);
}

function validateAddress(addr: string, requireCIDR: boolean): void {
  if (requireCIDR) {
    if (!isCIDRIPv4(addr)) {
      throw new Error("invalid zones IPv4 address " + JSON.stringify(addr) + " (want address/prefix)");
    }
    return;
  }
  if (isCIDRIPv4(addr)) return;
  if (!isIPv4(addr)) {
    throw new Error("invalid zones IPv4 address " + JSON.stringify(addr));
  }
}

function validateRouter(router: string): void {
  if (!isIPv4(router)) {
    throw new Error("invalid zones default router " + JSON.stringify(router));
  }
}

function validateDNSServers(): void {
  for (const server of dnsServers()) {
    if (!isIPv4(server)) {
      throw new Error("invalid zones DNS server IP " + JSON.stringify(server));
    }
  }
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

function validateZoneName(name: string): void {
  if (str(name).trim() === "") {
    throw new Error("zone name is required");
  }
  if (name.indexOf(zoneNamePrefix) !== 0) {
    throw new Error("zone name " + JSON.stringify(name) + " must start with " + JSON.stringify(zoneNamePrefix));
  }
  if (name.indexOf("..") !== -1 || name.indexOf("/") !== -1 || name.indexOf("\\") !== -1 || !safeNameRE.test(name)) {
    throw new Error("invalid zone name " + JSON.stringify(name));
  }
}

function validateZonePath(path: string): void {
  if (str(path).trim() === "") {
    throw new Error("zones zonepath is required (-zones-zonepath)");
  }
  if (path.indexOf("/") !== 0) {
    throw new Error("zones zonepath " + JSON.stringify(path) + " must be absolute");
  }
  if (path === "/") {
    throw new Error("refusing to use / as the zones zonepath");
  }
}

function validateInterfaceName(ifaceName: string): void {
  if (str(ifaceName).trim() === "") {
    throw new Error("zones network interface is required (-zones-interface)");
  }
  if (ifaceName.indexOf("..") !== -1 || /[\/\\ ]/.test(ifaceName) || !safeNameRE.test(ifaceName)) {
    throw new Error("invalid zones network interface " + JSON.stringify(ifaceName));
  }
}

function validateConfig(): void {
  validateZonePath(zonepath());
  const mode = networkMode();
  if (mode !== "shared" && mode !== "exclusive") {
    throw new Error("unsupported zones network mode " + JSON.stringify(mode) + " (want shared or exclusive)");
  }
  const base = ipBaseNetwork();
  const size = ipPoolSize();
  if ((base === "") !== (size === 0)) {
    throw new Error("zones IP base network and pool size must be set together");
  }
  if (base !== "") {
    if (!isCIDRIPv4(base)) {
      throw new Error("invalid zones IP base network " + JSON.stringify(base));
    }
    if (size < 1) {
      throw new Error("zones IP pool size must be positive, got " + size);
    }
  }
  validateDNSServers();
}

// --- zonecfg helpers ---

function zonecfgValue(value: string): string {
  if (value === "") {
    return '""';
  }
  if (!/[\s"'\\;]/.test(value)) {
    return value;
  }
  const escaped = value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  return '"' + escaped + '"';
}

interface zoneConfig {
  name: string;
  zonePath: string;
  brand: string;
  baseZone: string;
  networkMode: string;
  physical: string;
  address: string;
  defRouter: string;
  workDir: string;
  workspaceHostDir: string;
  cacheMountPath: string;
  removeNet: boolean;
  removeFS: boolean;
}

function buildZoneConfig(c: zoneConfig): string {
  const lines: string[] = [];
  if (c.baseZone !== "") {
    lines.push("create -t " + zonecfgValue(c.baseZone));
    if (c.removeNet) lines.push("remove net");
    if (c.removeFS) lines.push("remove fs");
  } else {
    lines.push("create");
    lines.push("set brand=" + zonecfgValue(c.brand));
  }
  lines.push(
    "set zonepath=" + zonecfgValue(c.zonePath),
    "set autoboot=false",
    "set ip-type=" + c.networkMode,
    "add net",
    "set physical=" + zonecfgValue(c.physical),
  );
  if (c.networkMode === "shared" && c.address !== "") {
    lines.push("set address=" + zonecfgValue(c.address));
    if (c.defRouter !== "") {
      lines.push("set defrouter=" + zonecfgValue(c.defRouter));
    }
  }
  lines.push("end");

  if (c.workspaceHostDir !== "" && c.workDir !== "") {
    lines.push(
      "add fs",
      "set dir=" + zonecfgValue(c.workDir),
      "set special=" + zonecfgValue(c.workspaceHostDir),
      "set type=lofs",
      "set options=rw",
      "end",
    );
  }
  if (c.cacheMountPath !== "") {
    lines.push(
      "add fs",
      "set dir=/cache",
      "set special=" + zonecfgValue(c.cacheMountPath),
      "set type=lofs",
      "set options=rw",
      "end",
    );
  }
  lines.push("verify", "commit");
  return lines.join("\n") + "\n";
}

// --- VNIC naming ---

function fnv1a32(s: string, seed: number): number {
  let h = seed >>> 0;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = (h * 0x01000193) >>> 0;
  }
  return h;
}

function stableDigest16(s: string): string {
  const a = fnv1a32(s, 0x811c9dc5);
  const b = fnv1a32(s, 0xdeadbeef);
  return ("00000000" + a.toString(16)).slice(-8) + ("00000000" + b.toString(16)).slice(-8);
}

function validDladmLinkName(name: string): boolean {
  if (name === "" || name.length >= 32) return false;
  if (name[0] >= "0" && name[0] <= "9") return false;
  const last = name[name.length - 1];
  if (last < "0" || last > "9") return false;
  let i = name.length - 1;
  while (i >= 0 && name[i] >= "0" && name[i] <= "9") i--;
  if (name.length - 1 - i > 1 && name[i + 1] === "0") return false;
  for (let j = 0; j < name.length; j++) {
    const ch = name[j];
    if ((ch >= "a" && ch <= "z") || (ch >= "A" && ch <= "Z") ||
      (ch >= "0" && ch <= "9") || ch === "_" || ch === ".") {
      continue;
    }
    return false;
  }
  return true;
}

// vnicNameForZone derives a stable, short VNIC name that always satisfies
// dladm_valid_linkname(3DADM): under 32 chars, not starting with a digit, and
// ending in a number without a leading zero.
function vnicNameForZone(zoneName: string): string {
  let base = zoneName.replace(/-/g, "");
  if (base.length > 20) {
    base = "z" + stableDigest16(zoneName);
  }
  if (!validDladmLinkName(base)) {
    base = base.replace(/[0-9]+$/, "") + "0";
  }
  return base;
}

// --- command helpers ---

function isBenignZoneError(stderr: string): boolean {
  const s = str(stderr).toLowerCase();
  if (s.indexOf("executable file not found") !== -1) {
    return false;
  }
  const markers = ["not found", "no such zone", "does not exist", "not installed", "not running", "already stopped", "already halted", "is not configured"];
  for (const marker of markers) {
    if (s.indexOf(marker) !== -1) return true;
  }
  return false;
}

function quoteArg(arg: string): string {
  if (arg === "") {
    return "''";
  }
  if (!/[\s"'\\$&|;<>()*?[\]{}!#~`]/.test(arg)) {
    return arg;
  }
  return "'" + arg.replace(/'/g, "'\\''") + "'";
}

// stripLeading removes leading slashes so a jail/zone-relative path can be
// joined onto a root.
function stripLeading(p: string): string {
  let out = p;
  while (out.indexOf("/") === 0) out = out.slice(1);
  return out;
}

// cleanAbsPath mirrors filepath.Clean("/"+workDir): an absolute, cleaned path.
function cleanAbsPath(p: string): string {
  let out = "/" + stripLeading(p);
  out = out.replace(/\/+/g, "/");
  if (out.length > 1 && out.slice(-1) === "/") out = out.slice(0, -1);
  return out;
}

function isDir(path: string): boolean {
  return host.run(["test", "-d", path]).exit_code === 0;
}

// --- zone lifecycle primitives ---

function globalZonePrimaryIPv4(): string {
  const res = host.run(escalate(["ipadm", "show-addr", "-p", "-o", "addr"]));
  if (res.exit_code !== 0) {
    throw new Error("failed to determine the global zone IPv4 address; pass --zones-ip-address: " + res.stderr);
  }
  for (const raw of res.stdout.split("\n")) {
    const line = raw.trim();
    if (line === "" || line.indexOf("127.") === 0) continue;
    const slash = line.indexOf("/");
    const ipStr = slash >= 0 ? line.slice(0, slash) : line;
    if (!isIPv4(ipStr)) continue;
    return line;
  }
  throw new Error("could not determine the global zone IPv4 address; pass --zones-ip-address");
}

function validateInterfaceExists(ifaceName: string): void {
  const res = host.run(escalate(["dladm", "show-link", "-p", "-o", "link"]));
  if (res.exit_code !== 0) {
    throw new Error("failed to list data links: " + res.stderr);
  }
  const links: string[] = [];
  for (const line of res.stdout.split("\n")) {
    const l = line.trim();
    if (l === "") continue;
    links.push(l);
    if (l === ifaceName) return;
  }
  throw new Error("zones interface " + JSON.stringify(ifaceName) + " does not exist; available data links: " + links.join(", ") + " (find it with 'dladm show-phys')");
}

function ensureVNIC(uplink: string, vnic: string): void {
  const list = host.run(escalate(["dladm", "show-vnic", "-p", "-o", "link"]));
  if (list.exit_code === 0) {
    for (const line of list.stdout.split("\n")) {
      if (line.trim() === vnic) return;
    }
  }
  const create = host.run(escalate(["dladm", "create-vnic", "-l", uplink, vnic]));
  if (create.exit_code !== 0) {
    if (create.stderr.toLowerCase().indexOf("already exists") !== -1) return;
    throw new Error("failed to create VNIC " + vnic + " on " + uplink + ": " + create.stderr);
  }
}

function deleteVNIC(vnic: string): void {
  if (vnic === "") return;
  const res = host.run(escalate(["dladm", "delete-vnic", vnic]));
  if (res.exit_code !== 0 && !isBenignZoneError(res.stderr)) {
    throw new Error("failed to delete VNIC " + vnic + ": " + res.stderr);
  }
}

function deleteZoneConfig(zoneName: string): void {
  host.run(escalate(["zonecfg", "-z", zoneName, "delete", "-F"]));
}

function zoneHasResource(zoneName: string, resType: string): boolean {
  const res = host.run(escalate(["zonecfg", "-z", zoneName, "info", resType]));
  if (res.exit_code !== 0) {
    throw new Error("failed to inspect zone " + zoneName + " resources: " + res.stderr);
  }
  return res.stdout.trim() !== "";
}

function applyZoneConfig(zoneName: string, script: string): void {
  const path = "/tmp/wsd-zonecfg-" + zoneName;
  const write = host.run(["tee", path], { stdin: script });
  if (write.exit_code !== 0) {
    throw new Error("failed to write temporary zonecfg file");
  }
  host.onFail(["rm", "-f", path]);
  const res = host.run(escalate(["zonecfg", "-z", zoneName, "-f", path]));
  host.run(["rm", "-f", path]);
  if (res.exit_code !== 0) {
    throw new Error("zonecfg failed for " + zoneName + ": " + res.stderr);
  }
}

function ensureZoneMountPoints(zonePath: string, workDir: string, cacheMountPath: string): void {
  const root = zonePath + "/root";
  const dirs: string[] = [];
  if (workDir !== "") dirs.push(cleanAbsPath(workDir));
  if (cacheMountPath !== "") dirs.push("/cache");
  if (dirs.length === 0) return;
  const res = host.run(escalate(["chroot", root, "/usr/bin/mkdir", "-p"].concat(dirs)));
  if (res.exit_code !== 0) {
    throw new Error("failed to create zone mount points: " + res.stderr);
  }
}

function resolvConf(): string {
  const servers = dnsServers();
  if (servers.length > 0) {
    let content = "";
    for (const server of servers) content += "nameserver " + server + "\n";
    return content;
  }
  const res = host.run(["cat", "/etc/resolv.conf"]);
  if (res.exit_code !== 0) {
    throw new Error("failed to read host resolver configuration: " + res.stderr);
  }
  return res.stdout;
}

function writeZoneResolvConf(zonePath: string): void {
  const data = resolvConf();
  const root = zonePath + "/root";
  const res = host.run(escalate(["chroot", root, "/bin/sh", "-c", "umask 022; /usr/bin/mkdir -p /etc; printf '%s' \"$1\" > /etc/resolv.conf", "mcwsd-dns", data]));
  if (res.exit_code !== 0) {
    throw new Error("failed to write zone resolver configuration: " + res.stderr);
  }
}

function configureExclusiveNetwork(zoneName: string, vnic: string, address: string, router: string): void {
  let script = "ipadm show-if " + vnic + " >/dev/null 2>&1 || ipadm create-ip " + vnic + "; ";
  if (address === "") {
    script += "ipadm show-addr " + vnic + "/v4 >/dev/null 2>&1 || ipadm create-addr -T dhcp " + vnic + "/v4";
  } else {
    script += "ipadm show-addr " + vnic + "/v4 >/dev/null 2>&1 || ipadm create-addr -T static -a " + address + " " + vnic + "/v4";
    if (router !== "") {
      script += "; route -p add default " + router + " 2>/dev/null || true";
    }
  }
  script += "; ifconfig " + vnic + " up";

  const deadline = Date.now() + bootTimeoutMs();
  let lastErr = "";
  for (;;) {
    const res = host.run(escalate(["zlogin", zoneName, "sh", "-c", quoteArg(script)]));
    if (res.exit_code === 0) return;
    lastErr = res.stderr;
    if (Date.now() > deadline) break;
    host.sleep(pollIntervalMs());
  }
  throw new Error("failed to configure exclusive networking in zone " + zoneName + ": " + lastErr);
}

function zoneState(zoneName: string): string {
  const res = host.run(escalate(["zoneadm", "list", "-pc"]));
  if (res.exit_code !== 0) {
    throw new Error(res.stderr);
  }
  for (const line of res.stdout.split("\n")) {
    const fields = line.split(":");
    if (fields.length >= 3 && fields[1] === zoneName) {
      return fields[2];
    }
  }
  throw new Error("zone " + zoneName + " not found");
}

function waitForZoneReady(zoneName: string): void {
  const deadline = Date.now() + bootTimeoutMs();
  let lastErr = "";
  for (;;) {
    try {
      const state = zoneState(zoneName);
      if (state === "running") {
        const res = host.run(escalate(["zlogin", zoneName, "true"]));
        if (res.exit_code === 0) return;
        lastErr = res.stderr;
      }
    } catch (e) {
      lastErr = str(e);
    }
    if (Date.now() > deadline) {
      if (lastErr !== "") {
        throw new Error("timed out waiting for zone " + zoneName + " to accept zlogin: " + lastErr + " (a golden zone that completed sysding is recommended)");
      }
      throw new Error("timed out waiting for zone " + zoneName + " to become ready (first boot may be waiting on sysding; use a golden zone for unattended provisioning)");
    }
    host.sleep(pollIntervalMs());
  }
}

function unmountZoneRoot(zoneName: string): void {
  const root = zonePathFor(zoneName) + "/root";
  host.run(escalate(["umount", root]));
}

function destroyCloneSnapshot(zoneName: string): void {
  const golden = zonePathFor(baseZone()).replace(/^\//, "");
  const res = host.run(escalate(["zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", golden]));
  if (res.exit_code !== 0) return;
  const prefix = zoneName + "_snap";
  for (const line of res.stdout.split("\n")) {
    const l = line.trim();
    const at = l.lastIndexOf("@");
    if (at < 0 || !l.slice(at + 1).startsWith(prefix)) continue;
    host.run(escalate(["zfs", "destroy", l]));
  }
}

function destroyZoneDatasets(zoneName: string): void {
  const dataset = zonePathFor(zoneName).replace(/^\//, "");
  host.run(escalate(["zfs", "destroy", "-r", dataset]));
  if (baseZone() !== "") {
    destroyCloneSnapshot(zoneName);
  }
}

function cleanupFailedProvision(zoneName: string): void {
  host.run(escalate(["zoneadm", "-z", zoneName, "halt"]));
  host.run(escalate(["zoneadm", "-z", zoneName, "unmount"]));
  unmountZoneRoot(zoneName);
  host.run(escalate(["zoneadm", "-z", zoneName, "uninstall", "-F"]));
  host.run(escalate(["zonecfg", "-z", zoneName, "delete", "-F"]));
  destroyZoneDatasets(zoneName);
  if (networkMode() === "exclusive") {
    try {
      deleteVNIC(vnicNameForZone(zoneName));
    } catch (e) {
      host.log(1, "failed to delete VNIC for zone " + zoneName + ": " + str(e));
    }
  }
}

function destroyZone(zoneName: string): void {
  validateZoneName(zoneName);

  const halt = host.run(escalate(["zoneadm", "-z", zoneName, "halt"]));
  if (halt.exit_code !== 0 && !isBenignZoneError(halt.stderr)) {
    host.log(1, "halt zone " + zoneName + " reported: " + halt.stderr + " (continuing)");
  }
  const unmount = host.run(escalate(["zoneadm", "-z", zoneName, "unmount"]));
  if (unmount.exit_code !== 0 && !isBenignZoneError(unmount.stderr)) {
    host.log(1, "unmount zone " + zoneName + " reported: " + unmount.stderr + " (continuing)");
  }
  unmountZoneRoot(zoneName);

  const uninstall = host.run(escalate(["zoneadm", "-z", zoneName, "uninstall", "-F"]));
  if (uninstall.exit_code !== 0 && !isBenignZoneError(uninstall.stderr)) {
    throw new Error("failed to uninstall zone " + zoneName + ": " + uninstall.stderr);
  }
  const del = host.run(escalate(["zonecfg", "-z", zoneName, "delete", "-F"]));
  if (del.exit_code !== 0 && !isBenignZoneError(del.stderr)) {
    throw new Error("failed to delete zone configuration " + zoneName + ": " + del.stderr);
  }

  destroyZoneDatasets(zoneName);

  if (networkMode() === "exclusive") {
    try {
      deleteVNIC(vnicNameForZone(zoneName));
    } catch (e) {
      host.log(1, "failed to delete VNIC for zone " + zoneName + ": " + str(e));
    }
  }
}

function handleFor(zoneName: string, workDir: string, vnic: string): Handle {
  return {
    native_id: zoneName,
    work_dir: workDir,
    username: "root",
    privilege: "root",
    os: "illumos",
    vnic: vnic,
  };
}

// --- transport payload ---

function buildZloginPayload(cmd: string[], workDir: string, env: string[]): string {
  const assigns: string[] = ["PATH=" + zonesPath];
  for (const e of env || []) {
    assigns.push(quoteArg(e));
  }
  const parts = cmd.map((arg) => quoteArg(arg));
  let payload = "env " + assigns.join(" ") + " " + parts.join(" ");
  if (workDir !== "") {
    payload = "cd " + quoteArg(workDir) + " && " + payload;
  }
  return payload;
}

const zonesPlugin: RuntimePlugin = {
  name: "zones",
  description: "illumos / OpenIndiana zones, driven through zonecfg(8)/zoneadm(8)/zlogin(1)",
  version: "1.0.0",
  os: "illumos",
  isolation: "zone",
  binaries: [
    "zoneadm", "zonecfg", "zlogin", "zfs", "dladm", "ipadm", "chroot", "umount",
    "mkdir", "tee", "rm", "test", "cat", "pfexec", "sudo", "doas",
  ],
  options: [
    { flag: "zones-zonepath", type: "string", default: "", usage: "Parent ZFS dataset for workspace zone roots, e.g. /rpool1/zones" },
    { flag: "zones-brand", type: "string", default: "ipkg", usage: "Zone brand used by zoneadm install" },
    { flag: "zones-base-zone", type: "string", default: "", usage: "Installed, halted golden zone cloned for each workspace; empty uses zoneadm install" },
    { flag: "zones-network-mode", type: "string", default: "shared", usage: "Zone network mode: shared or exclusive" },
    { flag: "zones-interface", type: "string", default: "", usage: "Global-zone data link for shared-IP zones, or the VNIC uplink for exclusive-IP zones" },
    { flag: "zones-ip-address", type: "string", default: "", usage: "Shared-IP address, or exclusive-IP static address/prefix; empty uses the global address (shared) or DHCP (exclusive)" },
    { flag: "zones-defrouter", type: "string", default: "", usage: "Default router for static zone addressing" },
    { flag: "ip-base-network", type: "string", default: "", usage: "Base network in CIDR notation for automatic address allocation, e.g. 192.168.1.200/24" },
    { flag: "ip-pool-size", type: "int", default: "0", usage: "Number of addresses to allocate from --ip-base-network" },
    { flag: "zones-dns-servers", type: "string", default: "", usage: "Comma-separated DNS server IPs for zones; defaults to the global zone resolver" },
    { flag: "zones-escalation", type: "string", default: "pfexec", usage: "Privilege escalation command for zonecfg/zoneadm/zlogin/dladm: pfexec, sudo, doas, or empty to run directly" },
    { flag: "zones-boot-timeout", type: "duration", default: "5m", usage: "How long to wait for a freshly booted zone to accept zlogin" },
    { flag: "zones-poll-interval", type: "duration", default: "2s", usage: "How often to probe a booting zone's readiness" },
  ],

  init(): void {
    if (escalationCommand() === "") return;
    const res = host.run(escalate(["zoneadm", "list", "-cv"]));
    if (res.exit_code !== 0) {
      throw new Error("privilege escalator " + escalationCommand() + " is unavailable: " + res.stderr);
    }
  },

  validate(): void {
    validateConfig();
    if (!isDir(zonepath())) {
      throw new Error("zones zonepath " + zonepath() + " is not a directory");
    }
    const zfs = host.run(escalate(["zfs", "list", "-H", "-o", "name", zonepath()]));
    if (zfs.exit_code !== 0) {
      throw new Error("zones zonepath " + zonepath() + " is not a ZFS dataset (create it with 'zfs create <pool>/zones'): " + zfs.stderr);
    }
    if (baseZone() === "") {
      const installer = defaultBrandDir + "/" + brand() + "/pkgcreatezone";
      if (host.run(["test", "-f", installer]).exit_code !== 0) {
        throw new Error("zone brand " + JSON.stringify(brand()) + " installer " + installer + " is missing; update the system/zones package or use a golden zone");
      }
    }
    if (escalationCommand() !== "") {
      const res = host.run(escalate(["zoneadm", "list", "-cv"]));
      if (res.exit_code !== 0) {
        throw new Error("privilege escalator " + escalationCommand() + " is unavailable: " + res.stderr);
      }
    }
    if (iface() !== "") {
      validateInterfaceExists(iface());
    }
  },

  provision(spec: ProvisionSpec): Handle {
    validateConfig();
    validateWorkspaceID(str(spec.workspace_id));

    const zoneName = zoneNamePrefix + str(spec.workspace_id);
    validateZoneName(zoneName);
    const zonePath = zonePathFor(zoneName);
    validateZonePath(zonePath);

    let mode = networkMode();
    let ifaceName = iface();
    let address = ipAddress();
    let router = defRouter();
    if (str(spec.interface) !== "") {
      ifaceName = str(spec.interface);
    }
    if (str(spec.ip4_address) !== "") {
      address = str(spec.ip4_address);
    }

    if (address === "" && hasIPPool()) {
      address = host.ipam.allocate(str(spec.workspace_id));
      if (mode === "exclusive" && router === "") {
        router = host.ipam.gateway();
      }
    }

    if (mode === "shared") {
      if (ifaceName === "") {
        throw new Error("zones shared networking requires a data link (-zones-interface)");
      }
      validateInterfaceName(ifaceName);
      if (address === "") {
        address = globalZonePrimaryIPv4();
      }
      validateAddress(address, false);
      if (router !== "") {
        validateRouter(router);
      }
    } else {
      if (ifaceName === "") {
        throw new Error("zones exclusive networking requires an uplink interface (-zones-interface)");
      }
      validateInterfaceName(ifaceName);
      if (address !== "") {
        validateAddress(address, true);
        if (router === "") {
          throw new Error("static exclusive-IP requires a default router (-zones-defrouter)");
        }
        validateRouter(router);
        if (dnsServers().length === 0) {
          throw new Error("static exclusive-IP requires explicit DNS servers (-zones-dns-servers)");
        }
      } else if (router !== "") {
        throw new Error("zones default router provided without an IPv4 address; omit it for DHCP");
      }
    }
    validateDNSServers();

    if (!isDir(zonepath())) {
      throw new Error("zones zonepath " + zonepath() + " is not a directory");
    }

    let workspaceHostDir = "";
    if (str(spec.workspace_base_path) !== "") {
      const base = str(spec.workspace_base_path);
      if (base.indexOf("/") !== 0) {
        throw new Error("workspace base path " + JSON.stringify(base) + " must be absolute for zones");
      }
      workspaceHostDir = base + "/" + str(spec.workspace_id);
      const mkdir = host.run(["mkdir", "-p", workspaceHostDir]);
      if (mkdir.exit_code !== 0) {
        throw new Error("failed to create workspace host directory " + workspaceHostDir + ": " + mkdir.stderr);
      }
    }
    if (str(spec.cache_mount_path) !== "" && str(spec.cache_mount_path).indexOf("/") !== 0) {
      throw new Error("cache mount path " + JSON.stringify(str(spec.cache_mount_path)) + " must be absolute for zones");
    }

    let vnic = "";
    if (mode === "exclusive") {
      vnic = vnicNameForZone(zoneName);
      ensureVNIC(ifaceName, vnic);
    }
    const physical = mode === "exclusive" ? vnic : ifaceName;

    // A previous failed run may have left a partial configuration behind.
    deleteZoneConfig(zoneName);

    let removeNet = false;
    let removeFS = false;
    if (baseZone() !== "") {
      removeNet = zoneHasResource(baseZone(), "net");
      removeFS = zoneHasResource(baseZone(), "fs");
    }

    const config = buildZoneConfig({
      name: zoneName,
      zonePath: zonePath,
      brand: brand(),
      baseZone: baseZone(),
      networkMode: mode,
      physical: physical,
      address: address,
      defRouter: router,
      workDir: str(spec.work_dir),
      workspaceHostDir: workspaceHostDir,
      cacheMountPath: str(spec.cache_mount_path),
      removeNet: removeNet,
      removeFS: removeFS,
    });

    try {
      applyZoneConfig(zoneName, config);
      if (baseZone() !== "") {
        const clone = host.run(escalate(["zoneadm", "-z", zoneName, "clone", "-X", baseZone()]));
        if (clone.exit_code !== 0) {
          throw new Error("failed to clone zone " + zoneName + " from " + baseZone() + ": " + clone.stderr);
        }
      } else {
        const install = host.run(escalate(["zoneadm", "-z", zoneName, "install"]));
        if (install.exit_code !== 0) {
          throw new Error("failed to install zone " + zoneName + ": " + install.stderr);
        }
      }

      ensureZoneMountPoints(zonePath, str(spec.work_dir), str(spec.cache_mount_path));
      writeZoneResolvConf(zonePath);

      if (baseZone() !== "") {
        const unmount = host.run(escalate(["zoneadm", "-z", zoneName, "unmount"]));
        if (unmount.exit_code !== 0) {
          throw new Error("failed to unmount cloned zone " + zoneName + ": " + unmount.stderr);
        }
      }

      const boot = host.run(escalate(["zoneadm", "-z", zoneName, "boot"]));
      if (boot.exit_code !== 0) {
        throw new Error("failed to boot zone " + zoneName + ": " + boot.stderr);
      }

      waitForZoneReady(zoneName);

      if (mode === "exclusive") {
        configureExclusiveNetwork(zoneName, vnic, address, router);
      }
    } catch (e) {
      cleanupFailedProvision(zoneName);
      throw e;
    }

    return handleFor(zoneName, str(spec.work_dir), vnic);
  },

  destroy(handle: Handle): void {
    destroyZone(str(handle.native_id));
  },

  destroyByID(id: string): void {
    destroyZone(id);
  },

  destroyWorkspaceByID(workspaceID: string): void {
    validateWorkspaceID(workspaceID);
    destroyZone(zoneNamePrefix + workspaceID);
  },

  recover(nativeID: string, workDir: string): Handle {
    validateZoneName(nativeID);
    const state = zoneState(nativeID);
    switch (state) {
      case "running":
        break;
      case "ready":
        waitForZoneReady(nativeID);
        break;
      case "installed":
      case "down": {
        const boot = host.run(escalate(["zoneadm", "-z", nativeID, "boot"]));
        if (boot.exit_code !== 0) {
          throw new Error("failed to boot recovered zone " + nativeID + ": " + boot.stderr);
        }
        waitForZoneReady(nativeID);
        break;
      }
      default:
        throw new Error("zone " + nativeID + " is in state " + JSON.stringify(state) + " and cannot be recovered");
    }
    const vnic = networkMode() === "exclusive" ? vnicNameForZone(nativeID) : "";
    return handleFor(nativeID, workDir, vnic);
  },

  cleanupOrphans(keep: string[]): number {
    const keepSet: Record<string, boolean> = {};
    for (const name of keep) keepSet[name] = true;
    const names: Record<string, boolean> = {};
    if (zonepath() !== "") {
      const ls = host.run(["ls", "-1", zonepath()]);
      if (ls.exit_code === 0) {
        for (const raw of ls.stdout.split("\n")) {
          const name = raw.trim();
          if (name !== "" && name.indexOf(zoneNamePrefix) === 0 && isDir(zonepath() + "/" + name)) {
            names[name] = true;
          }
        }
      }
    }
    const list = host.run(escalate(["zoneadm", "list", "-pc"]));
    if (list.exit_code === 0) {
      for (const line of list.stdout.split("\n")) {
        const fields = line.split(":");
        if (fields.length >= 2 && fields[1].indexOf(zoneNamePrefix) === 0) {
          names[fields[1]] = true;
        }
      }
    }
    let removed = 0;
    for (const name in names) {
      if (keepSet[name] || name === baseZone()) continue;
      try {
        destroyZone(name);
        removed++;
      } catch (e) {
        host.log(1, "failed to remove orphan zone " + name + ": " + str(e));
      }
    }
    return removed;
  },

  execArgs(handle: Handle, cmd: string[], workDir: string, env: string[], opts: ExecArgsOptions): string[] | { argv: string[]; stdin?: string } {
    const zone = str(handle.native_id);
    if (opts.stdin) {
      // Core is streaming data (WriteFile content or CopyDirIn tar).
      if (cmd.length >= 2 && cmd[0] === "tar" && cmd[1] === "-xf") {
        return escalate(["zlogin", zone].concat(cmd));
      }
      // WriteFile: core runs ["sh","-c","cat > \"$1\"","sh",path]; zones uses
      // tee(1) instead so the content lands verbatim without a shell.
      const path = cmd[cmd.length - 1];
      return escalate(["zlogin", zone, "tee", path]);
    }
    // Plain command: zlogin re-parses argv through su -c and mangles shell
    // metacharacters, so stream the script to `sh -s` on stdin instead.
    return {
      argv: escalate(["zlogin", zone, "sh", "-s"]),
      stdin: buildZloginPayload(cmd, workDir, env) + "\n",
    };
  },

  setNetwork(handle: Handle, enabled: boolean): void {
    if (networkMode() !== "exclusive") {
      return;
    }
    const vnic = str(handle.vnic);
    if (vnic === "") {
      throw new Error("exclusive-IP zone " + str(handle.native_id) + " has no VNIC");
    }
    const state = enabled ? "up" : "down";
    const res = host.run(escalate(["zlogin", str(handle.native_id), "ifconfig", vnic, state]));
    if (res.exit_code !== 0) {
      throw new Error("failed to set zone network " + state + ": " + res.stderr);
    }
  },

  bootstrapCmd(): string[] | null {
    return ["sh", "-c", bootstrapScript];
  },
};

module.exports = zonesPlugin;
