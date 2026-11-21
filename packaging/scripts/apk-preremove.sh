#!/bin/sh
# mcwsd preremove for Alpine (.apk / OpenRC).
set -e

if [ -x /etc/init.d/mcwsd ]; then
    rc-service mcwsd stop >/dev/null 2>&1 || true
    rc-update del mcwsd default >/dev/null 2>&1 || true
fi

exit 0
