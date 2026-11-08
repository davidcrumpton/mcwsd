# Systemd Setup for the Workspace Daemon

This guide covers running the workspace daemon (`mcwsd`) as a systemd service under a dedicated `_wsd` user.

## Prerequisites

- Docker installed and running
- The `mcwsd` binary installed at `/usr/local/bin/mcwsd`
- A `_wsd` system user (with access to the Docker socket)
- TLS certificate and key, plus a GitLab token and SSH key for the forge

## 1. Paths and Ownership

Create the directories the daemon needs and give `_wsd` ownership:

```bash
sudo mkdir -p /var/lib/wsd /etc/wsd /home/wsd/workspaces
sudo chown -R _wsd:_wsd /var/lib/wsd /etc/wsd /home/wsd/workspaces
```

| Path                     | Purpose                                  |
| ------------------------ | ---------------------------------------- |
| `/var/lib/wsd`           | Working directory, cache, state file     |
| `/etc/wsd`               | SSH key, known hosts, TLS certificates   |
| `/home/wsd/workspaces`   | Base path for provisioned workspaces     |

## 2. Start Script

Create `/var/lib/wsd/startmcwsd`. It sets the environment and launches the daemon with the desired configuration.

```bash
#!/bin/sh

# Secrets and credentials
export MCWSD_GITLAB_TOKEN=glpat-<token>
export MCWSD_TOKEN=wsd-<token>
export MCWSD_GIT_SSH_KEY_PATH=/etc/wsd/wsd_rsa
export MCWSD_GIT_SSH_KNOWN_HOSTS=/etc/wsd/known_hosts

exec /usr/local/bin/mcwsd daemon \
    --port 9080 \
    --bind-address=0.0.0.0 \
    --plugin /usr/local/lib/mcwsd/plugins/docker.js \
    --workspace-base-path /home/wsd/workspaces \
    --cache-dir /var/lib/wsd/cache \
    --state-file /var/lib/wsd/state.json \
    --tls-cert-file /etc/wsd/ssl/fullchain.pem \
    --tls-key-file /etc/wsd/ssl/key.pem \
    --forge-type=gitlab \
    --gitlab-api-base https://gitlab.crumpton.org/api/v4 \
    --gitlab-token-env-var=MCWSD_GITLAB_TOKEN \
    --git-auth-mode=ssh \
    --git-ssh-key-path-env-var=MCWSD_GIT_SSH_KEY_PATH \
    --git-ssh-known-hosts-env-var=MCWSD_GIT_SSH_KNOWN_HOSTS \
    --debug
```

Make it executable and keep it private, since it contains tokens:

```bash
sudo chown _wsd:_wsd /var/lib/wsd/startmcwsd
sudo chmod 700 /var/lib/wsd/startmcwsd
```

> **Note:** `--debug` and `--bind-address=0.0.0.0` are convenient for initial setup. Consider dropping `--debug` and binding to a specific interface once things are stable.

## 3. Systemd Service Unit

Create `/etc/systemd/system/mcwsd.service`:

```ini
[Unit]
Description=Workspace Daemon
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
Type=simple
User=_wsd
WorkingDirectory=/var/lib/wsd
ExecStart=/bin/sh /var/lib/wsd/startmcwsd
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s

[Install]
WantedBy=multi-user.target
```

## 4. Enable and Start

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mcwsd.service
```

Check status and logs:

```bash
systemctl status mcwsd.service
journalctl -u mcwsd.service -f
```