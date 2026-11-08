/**
 * mcwsd runtime plugin type definitions.
 *
 * A runtime plugin is a single CommonJS module exporting a RuntimePlugin. It is
 * compiled from TypeScript to a goja-compatible .js file (esbuild, cjs,
 * es2015). One plugin backs one daemon instance; the operator runs separate
 * instances in separate accounts/paths for separate runtimes.
 *
 * goja constraints:
 *   - No async/await. Every function is synchronous; host.* calls block.
 *   - No fetch / URL / Node.js builtins — use host.fetch / host.run.
 *   - transport functions (execArgs, setNetworkArgs, bootstrapCmd) must be
 *     pure: no host.run / host.sleep / host.onFail side effects. They map
 *     (handle, argv) -> host argv. Core runs the result and implements
 *     ReadFile/WriteFile/CopyDirIn/Bootstrap on top of execArgs.
 *   - lifecycle functions (provision, destroy, ...) may use the host API, but
 *     must not keep per-workspace state on module.exports — use the handle
 *     object and host.ipam (owned by core) instead.
 */

// ---------------------------------------------------------------------------
// Host API (the global `host`)
// ---------------------------------------------------------------------------

export interface CommandResult {
  stdout: string;
  stderr: string;
  exit_code: number;
}

export interface HostIPAM {
  allocate(workspaceID: string): string;
  release(workspaceID: string): void;
  reserve(workspaceID: string, addr: string): void;
  gateway(): string;
  network(): string;
}

export interface HostFS {
  write(path: string, content: string): void;
  read(path: string): string;
}

export interface Host {
  /** Run argv on the host. argv[0] must be in the plugin's `binaries` list.
   *  A non-zero exit is returned in exit_code; a start failure throws. */
  run(argv: string[], opts?: { stdin?: string; input?: string }): CommandResult;
  /** Sleep for ms milliseconds, honoring request cancellation (throws on cancel). */
  sleep(ms: number): void;
  log(level: number, msg: string): void;
  env(name: string): string;
  /** Register an undo step: argv is replayed (in reverse) if provision fails. */
  onFail(argv: string[]): void;
  /** Resolved plugin options (from --help flags), keyed by flag name. */
  options: Record<string, unknown>;
  ipam: HostIPAM;
  /** Download a URL to a scratch file, returning its absolute path.
   *  opts may carry sha256 (verify digest), username/password_env (HTTP Basic),
   *  header/header_env (custom auth header), and public_key (verify the
   *  detached OpenPGP signature at url+".sig"). Credentials are referenced by
   *  environment-variable name, never embedded. */
  fetch(
    url: string,
    opts?: {
      sha256?: string;
      username?: string;
      password_env?: string;
      header?: string;
      header_env?: string;
      public_key?: string;
      timeout_seconds?: number;
    },
  ): string;
  /** File I/O confined to a per-call scratch root (never the host root). */
  fs: HostFS;
}

// ---------------------------------------------------------------------------
// Plugin shape
// ---------------------------------------------------------------------------

export interface PluginOption {
  /** Long flag name without leading dashes, e.g. "docker-network". */
  flag: string;
  type: "string" | "int" | "bool" | "duration";
  default?: string;
  usage: string;
}

export interface ProvisionSpec {
  workspace_id: string;
  image: string;
  work_dir: string;
  cache_mount_path: string;
  workspace_base_path: string;
  /** reserved word is fine as an object key in ES5+. */
  interface: string;
  ip4_address: string;
}

export interface Handle {
  native_id: string;
  work_dir: string;
  username: string;
  privilege: "root" | "user";
  /** Plugins may carry extra per-workspace connection details here (ip, port,
   *  user, keyPath, networkMode, vnic, ...). They are passed back verbatim to
   *  execArgs / setNetworkArgs / bootstrapCmd. */
  [key: string]: unknown;
}

export interface ExecArgsOptions {
  stdin: boolean;
  /** true when core is running its own privileged file I/O (ReadFile/
   *  WriteFile/CopyDirIn), so the plugin can run those as root while leaving
   *  user commands at the image default user. */
  root: boolean;
}

export interface RuntimePlugin {
  name: string;
  description: string;
  version: string;
  os: string;
  isolation: string;
  options: PluginOption[];
  /** Host binaries this plugin may execute (reviewable gate). */
  binaries: string[];

  init?(): void;
  validate?(): void;

  provision(spec: ProvisionSpec): Handle;
  destroy(handle: Handle): void;
  destroyByID(id: string): void;
  destroyWorkspaceByID?(id: string): void;
  recover?(nativeID: string, workDir: string): Handle;
  cleanupOrphans?(keep: string[]): number;

  /** Map (handle, cmd, workDir, env) to the host argv that runs cmd. Pure.
   *  May instead return { argv, stdin } to stream the command script on stdin
   *  (used by runtimes whose transport mangles argv, e.g. zlogin `sh -s`). */
  execArgs(
    handle: Handle,
    cmd: string[],
    workDir: string,
    env: string[],
    opts: ExecArgsOptions,
  ): string[] | { argv: string[]; stdin?: string };
  /** Toggle network access. May use host.run (this is a lifecycle function,
   *  not a pure argv mapping) so it can tolerate idempotency quirks. */
  setNetwork?(handle: Handle, enabled: boolean): void;
  /** Return the in-runtime argv to run once after provision, or null. Pure. */
  bootstrapCmd?(handle: Handle, workDir: string): string[] | null;
}

// ---------------------------------------------------------------------------
// Globals injected by the mcwsd loader
// ---------------------------------------------------------------------------

declare global {
  const host: Host;
}
