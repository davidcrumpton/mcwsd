#!/bin/sh
# mcwsd postinstall for .deb / .rpm.
#
# Creates the _wsd service account and its directories, reloads systemd, and
# prints what still has to be done by hand. Deliberately does NOT enable or
# start the service: the daemon needs MCWSD_TOKEN in /etc/mcwsd/mcwsd.env first.
set -e

if command -v systemd-sysusers >/dev/null 2>&1; then
    systemd-sysusers mcwsd.conf >/dev/null 2>&1 || true
fi

if command -v systemd-tmpfiles >/dev/null 2>&1; then
    systemd-tmpfiles --create mcwsd.conf >/dev/null 2>&1 || true
fi

# Give the daemon access to the container runtime socket if one is present.
# This grants _wsd the equivalent of root on this host; skip it and run the
# daemon under a runtime that does not need the socket if that is unacceptable.
if getent group docker >/dev/null 2>&1; then
    if command -v usermod >/dev/null 2>&1; then
        usermod -aG docker _wsd >/dev/null 2>&1 || true
    fi
fi
if getent group podman >/dev/null 2>&1; then
    if command -v usermod >/dev/null 2>&1; then
        usermod -aG podman _wsd >/dev/null 2>&1 || true
    fi
fi

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'

mcwsd installed.

  binary   /usr/bin/mcwsd
  plugins  /usr/share/mcwsd/plugins/*.js
  config   /etc/mcwsd/mcwsd.env
  unit     /usr/lib/systemd/system/mcwsd.service
  state    /var/lib/mcwsd   (owned by _wsd)
  logs     /var/log/mcwsd

Next steps:
  1. Generate a client token:      mcwsd token
  2. Put it in /etc/mcwsd/mcwsd.env as MCWSD_TOKEN=...
  3. Pick a runtime plugin in the unit's ExecStart (--plugin ...), e.g.
     docker.js, podman.js or kubernetes.js
  4. Start it:                     systemctl enable --now mcwsd
  5. Check it:                     mcwsd status --plugin /usr/share/mcwsd/plugins/docker.js

The service is not enabled automatically because it cannot start without a token.

EOF

exit 0
