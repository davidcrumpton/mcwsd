# Runtime plugins

A **runtime plugin** is a single JavaScript file (authored in TypeScript, then
compiled) that supplies both the *lifecycle* and the *transport* for one
execution runtime — podman, docker, FreeBSD jails, OpenBSD vmd, illumos zones,
Kubernetes, and anything else. It runs inside the daemon in a `goja` VM and is
given a small, reviewable host API instead of raw access to the machine.

The native Go backends in `internal/backend/*` have been ported to these
plugins one at a time; `podman`, `docker`, `kubernetes`, `openbsd-vmd`,
`freebsd-jail`, and `zones` are all available in `plugins/`.

## One plugin per daemon instance

mcwsd deliberately does **not** support loading multiple plugins at once. A
daemon instance runs exactly one runtime. To run more than one runtime, run a
separate `mcwsd daemon` per runtime, each with its own:

- account / privileges (a jail daemon needs `mdo`, a VM daemon needs `doas`,
  a container daemon needs none);
- state file and cache directory (`--state-file`, `--cache-dir`, `--jobs-file`);
- port and token.

This keeps the plugin's allowed-binary list and privilege boundary tied to one
account, so a root-capable plugin stays reviewable as one unit.

```bash
mcwsd daemon --plugin plugins/podman.js --port 9080 --state-file ~/wsd/podman/state.json
mcwsd daemon --plugin plugins/docker.js --port 9081 --state-file ~/wsd/docker/state.json
```

## Anatomy of a plugin

A plugin is a CommonJS module exporting a `RuntimePlugin` object (see
`plugin-devel/types/mcwsd-runtime.d.ts` for the full contract):

```ts
module.exports = {
  name: "docker",
  os: "linux",          // reported to the agent (package manager selection)
  isolation: "container", // container | jail | vm | zone | process
  binaries: ["docker", "mkdir"], // the only host binaries this plugin may run
  options: [            // surfaced as `mcwsd daemon --help` flags
    { flag: "docker-network", type: "string", default: "bridge", usage: "..." },
  ],

  // --- lifecycle (uses the host API; one fresh VM per call) ---
  provision(spec) { ... return handle; },
  destroy(handle) { ... },
  destroyByID(id) { ... },
  setNetwork(handle, enabled) { ... },   // optional
  recover(nativeID, workDir) { ... },     // optional
  destroyWorkspaceByID(id) { ... },       // optional
  cleanupOrphans(keep) { ... },           // optional
  validate() { ... },                     // optional
  init() { ... },                         // optional

  // --- transport (pure: argv in, argv out) ---
  execArgs(handle, cmd, workDir, env, opts) { ... return argv; },
  bootstrapCmd(handle, workDir) { ... return argv|null; }, // optional
};
```

### Lifecycle functions

`provision`, `destroy`, `destroyByID`, `setNetwork`, `recover`,
`destroyWorkspaceByID`, `cleanupOrphans`, `validate`, and `init` are ordinary
synchronous functions. They use the `host` API (below), must **not** keep
per-workspace state on `module.exports`, and run in a fresh VM per call so
concurrent provisions cannot leak state into one another. Shared state (the IP
pool, options) lives in core, not in the plugin.

On a failed `provision`, the plugin should register undo steps with
`host.onFail(argv)`; core replays them in reverse (best-effort). This replaces
the hand-written `cleanupFailedProvision` logic every native backend used to
need.

### Transport functions are pure

`execArgs(handle, cmd, workDir, env, opts)` maps "a command to run inside the
runtime" to the **host argv** that runs it. `opts.stdin` is true when core will
stream stdin (tar, `cat > "$1"`); `opts.root` is true when core is doing its
own privileged file I/O (ReadFile/WriteFile/CopyDirIn). It must be pure — no
`host.run`, no `host.sleep` — and is invoked on the hot path, so it lives in a
long-lived VM.

It may return either a plain argv array or `{ argv, stdin }`. The object form
exists for runtimes whose transport mangles argv (e.g. zlogin's `su -c`
re-parsing), so the command script is streamed on stdin to `sh -s` instead of
embedded in argv; when set, that stdin overrides the data stdin core would
otherwise stream.

Core owns the generic implementations of `ReadFile` (`cat`), `WriteFile`
(`cat > "$1"` over stdin), `CopyDirIn` (host `tar` streamed to `tar -xf -`) and
`Bootstrap`, all built on top of `execArgs`. The plugin never re-implements
these per runtime.

```ts
execArgs(handle, cmd, workDir, env, opts) {
  const args = ["podman", "exec"];
  if (opts.stdin) args.push("-i");
  if (opts.root) args.push("--user", "0");
  args.push("--workdir", workDir);
  for (const e of env) args.push("--env", e);
  args.push(handle.native_id);
  return args.concat(cmd);
}
```

## Host API

The global `host` available to lifecycle functions (and read-only in transport
functions via `host.options`):

| Member | Description |
| ------ | ----------- |
| `host.run(argv, { stdin })` | Run argv on the host. `argv[0]` must be in `binaries`. Non-zero exit returns in `exit_code`; a start failure throws. |
| `host.onFail(argv)` | Register an undo step, replayed in reverse on provision failure. |
| `host.sleep(ms)` | Sleep, honoring request cancellation. |
| `host.log(level, msg)` | Log (1 warn, 2 info, 3 error). |
| `host.env(name)` | Read a host environment variable. |
| `host.options` | Resolved plugin options, keyed by flag name. |
| `host.ipam` | `allocate/release/reserve/gateway/network` — the shared IP pool (core-owned). |
| `host.fetch(url, { sha256, username, password_env, header, header_env, public_key })` | Download to a scratch file, returning its path. Options add a checksum, HTTP Basic/header auth (env-var names, never values), and detached OpenPGP signature verification of `url+".sig"`. |
| `host.fs.write/read` | File I/O confined to a per-call scratch root. |

`host.run` never accepts shell strings — argv only — and rejects any binary
outside the plugin's `binaries` list. That is the gate that makes a
root-capable plugin reviewable: an operator can read the `binaries` list and
know exactly what it may execute.

## Options bubble up to `--help`

A plugin declares its administrator-facing knobs in `options`; core registers
one flag per entry, so the selected plugin's flags appear in
`mcwsd daemon --help`:

```
$ mcwsd daemon --plugin plugins/docker.js --help
  --docker-network string   Docker network containers attach to for
                            provisioning and exec network access (default "bridge")
  --plugin string           Path to a single .js runtime plugin ...
```

Supported types are `string`, `int`, `bool`, and `duration`. A duplicate flag
name (or a collision with a core flag) is a startup error. Two reserved flags,
`--ip-base-network` and `--ip-pool-size`, configure the shared IP pool in core
when both are declared.

## Authoring workflow

```bash
cd plugin-devel
npm install

# write a plugin at plugin-devel/plugins/<name>/plugin.ts

npm run build          # compile TypeScript -> dist/<name>.js
npm test               # vitest unit tests (mock the host global)
npm run lint           # tsc --noEmit
npm run deploy         # copy dist/*.js -> ../plugins/
```

Then run:

```bash
mcwsd daemon --plugin plugins/<name>.js --debug
```

## What stays in Go

The provision pipeline, job supervisor, state and job stores, `gitpush`, the
secrets broker, and the daemon's invariants (network off by default, detach
with a non-cancelled context, `resolveWorkspacePath`) remain in Go. A plugin
cannot weaken them: it is handed argv-only execution, confined file writes, and
a declared binary list.

## Hard-won quirks stay, just relocated

The platform-specific knowledge that was previously inline in each backend —
`jail -m name=` form, `certctl rehash` before `pkg bootstrap`, the pkg DB
ownership, the OpenBSD three-stage SSH readiness poll, zlogin's `su -c`
argument mangling — moves into the plugin but does not disappear. Keep the
existing `*_test.go` golden tests as the trace of those quirks, and port them
to the plugin's vitest suite as each runtime is migrated.

## Reference ports

- `plugins/podman.js` — `plugins/podman/plugin.ts` (simplest CLI runtime, no options).
- `plugins/docker.js` — `plugins/docker/plugin.ts` (demonstrates `options`, `setNetwork`, `bootstrapCmd`).
- `plugins/kubernetes.js` — `plugins/kubernetes/plugin.ts` (demonstrates `init`, `recover`, a poll-based `provision`, and shell-quoted `execArgs` through `kubectl exec ... -- sh -c`).
- `plugins/openbsd-vmd.js` — `plugins/openbsd-vmd/plugin.ts` (a VM runtime: SSH transport via `execArgs`, a three-stage readiness poll, host/guest escalation, and a per-handle `os` override).
- `plugins/freebsd-jail.js` — `plugins/freebsd-jail/plugin.ts` (a jail runtime: `jexec` transport, shared/VNET networking, the reserved `ip-base-network`/`ip-pool-size` pool, `base.txz` + template download via `host.fetch` with Basic/header auth and OpenPGP signature verification).
- `plugins/zones.js` — `plugins/zones/plugin.ts` (an illumos zone runtime: `zlogin` transport that streams commands to `sh -s` via the `{argv, stdin}` execArgs form, shared/exclusive networking, VNIC management, and the reserved `ip-base-network`/`ip-pool-size` pool).

All runtimes have been ported to plugins; the native Go provisioners that
remain (`docker`, `podman`, `kubernetes`) are themselves thin and can be
treated as reference implementations of the same contract.
