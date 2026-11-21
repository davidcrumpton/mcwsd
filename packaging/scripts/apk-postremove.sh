#!/bin/sh
# mcwsd postremove for Alpine (.apk / OpenRC).
#
# State, logs, config and the _wsd user are left behind on purpose: they hold
# workspaces, audit history and credentials that an upgrade must not destroy.
set -e

cat <<'EOF'
mcwsd removed. Left in place on purpose:
  /var/lib/mcwsd  (workspaces, cache, state)
  /var/log/mcwsd  (audit log)
  /etc/mcwsd      (configuration and secrets)
  the _wsd system user

EOF

exit 0
