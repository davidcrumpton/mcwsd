#!/bin/sh
# mcwsd postremove for .deb / .rpm.
#
# Reloads systemd after the unit is gone. State (/var/lib/mcwsd), logs
# (/var/log/mcwsd), config (/etc/mcwsd) and the _wsd user are intentionally
# left behind: they hold workspaces, audit history and credentials that a
# package upgrade must not destroy. Remove them by hand if you mean it.
set -e

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'
mcwsd removed. Left in place on purpose:
  /var/lib/mcwsd  (workspaces, cache, state)
  /var/log/mcwsd  (audit log)
  /etc/mcwsd      (configuration and secrets)
  the _wsd system user

EOF

exit 0
