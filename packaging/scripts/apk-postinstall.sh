#!/bin/sh
# mcwsd postinstall for Alpine (.apk / OpenRC).
#
# Creates the _wsd service account and its directories, then prints the manual
# steps. Does not start the service: MCWSD_TOKEN has to be set first.
set -e

if ! getent group _wsd >/dev/null 2>&1; then
    addgroup -S _wsd >/dev/null 2>&1 || true
fi

if ! getent passwd _wsd >/dev/null 2>&1; then
    adduser -S -D -H \
        -h /var/lib/mcwsd \
        -s /sbin/nologin \
        -G _wsd \
        -g "mcwsd workspace daemon" \
        _wsd >/dev/null 2>&1 || true
fi

# Give the daemon access to the container runtime socket if one is present.
if getent group docker >/dev/null 2>&1; then
    addgroup _wsd docker >/dev/null 2>&1 || true
fi
if getent group podman >/dev/null 2>&1; then
    addgroup _wsd podman >/dev/null 2>&1 || true
fi

for d in /var/lib/mcwsd /var/lib/mcwsd/cache /var/lib/mcwsd/workspaces /var/log/mcwsd; do
    mkdir -p "$d"
    chown _wsd:_wsd "$d"
    chmod 0750 "$d"
done
mkdir -p /run/mcwsd
chown _wsd:_wsd /run/mcwsd

cat <<'EOF'

mcwsd installed.

  binary   /usr/bin/mcwsd
  plugins  /usr/share/mcwsd/plugins/*.js
  config   /etc/mcwsd/mcwsd.env
  init     /etc/init.d/mcwsd  (settings in /etc/conf.d/mcwsd)
  state    /var/lib/mcwsd   (owned by _wsd)
  logs     /var/log/mcwsd

Next steps:
  1. Generate a client token:      mcwsd token
  2. Put it in /etc/mcwsd/mcwsd.env as MCWSD_TOKEN=...
  3. Pick a runtime plugin in /etc/conf.d/mcwsd (MCWSD_PLUGIN=...)
  4. Start it:                     rc-update add mcwsd default && rc-service mcwsd start

EOF

exit 0
