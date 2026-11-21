#!/bin/sh
# mcwsd container entrypoint.
#
#   container run mcwsd-kubernetes                 # daemon, Kubernetes plugin
#   container run mcwsd-kubernetes --port 9081     # extra daemon flags
#   container run mcwsd-kubernetes daemon --debug  # explicit daemon flags
#   container run mcwsd-kubernetes token           # any mcwsd subcommand
#   container run mcwsd-kubernetes sh              # debug shell
set -eu

plugin="${MCWSD_PLUGIN:-/usr/local/lib/mcwsd/plugins/kubernetes.js}"

case "${1:-}" in
  token|version)
    # These subcommands do not load a runtime plugin.
    exec mcwsd "$@"
    ;;
  cleanup|status)
    # Both derive their per-plugin state file from --plugin.
    sub="$1"
    shift
    exec mcwsd "$sub" --plugin "$plugin" "$@"
    ;;
  daemon)
    # Explicit daemon invocation: drop the subcommand and fall through to the
    # shared default flags below.
    shift
    ;;
  ""|-*)
    # No subcommand (or only flags): use the daemon defaults.
    ;;
  *)
    # Anything else is treated as a command, which keeps `sh`, `id`, and
    # `kubectl` usable for debugging without a second image.
    exec "$@"
    ;;
esac

# Default daemon invocation. Flags supplied by the caller are appended, so
# pflag's last-one-wins lets them override the defaults below (for example
# --port, --workspace-auth-mode, or --kubernetes-namespace). Use MCWSD_PLUGIN
# to point at a different plugin file.
exec mcwsd daemon \
  --plugin "$plugin" \
  --bind-address=0.0.0.0 \
  --port=9080 \
  --workspace-auth-mode=scoped \
  "$@"
