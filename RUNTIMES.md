# Inflexible Runtimes

They share the same path so switching leaves a mess behind.

The session below is switched between docker and podman. In a future release, we will refactor the code so state is prefixed by plugin name.


```sh
bear@bbgamer:~/Workspace/mcwsd$ ~/mcwsd daemon --plugin plugins/docker.js --debug
2026-11-07T19:13:09-07:00 [DEBUG] command_start command=mcwsd daemon
2026-11-07T19:13:09-07:00 [INFO] session_started application_name=mcwsd version=0.4.000 session_id=048900910453
2026-11-07T19:13:09-07:00 [WARN] git_auth_not_configured detail=running in forgeless mode; blank workspaces are supported, repository preparation and commit_and_pr require forge authentication
2026-11-07T19:13:09-07:00 [INFO] using_script_plugin plugin=docker os=linux isolation=container
2026-11-07T19:13:09-07:00 [INFO] provisioner_validated runtime=default
2026-11-07T19:13:09-07:00 [INFO] provisioner_initialized runtime=default
2026-11-07T19:13:09-07:00 [INFO] job_limits_configured max_per_workspace=1 max_total=0 max_runtime=0s max_log_bytes=10485760 retention=24h0m0s
2026-11-07T19:13:09-07:00 [INFO] workspace_recovery_started
2026-11-07T19:13:09-07:00 [DEBUG] workspace_recovery_skipped workspace_id=ws-b01b61037 container_id=cf10c2c62cf4b7464c8d51171f261d06e42b9af7bf8661911ff5ba526bfb60c3 reason=plugin recover: Error: container cf10c2c62cf4b7464c8d51171f261d06e42b9af7bf8661911ff5ba526bfb60c3 not found: error: no such object: cf10c2c62cf4b7464c8d51171f261d06e42b9af7bf8661911ff5ba526bfb60c3
 at recover (<eval>:64:13(26))
2026-11-07T19:13:09-07:00 [DEBUG] workspace_recovery_skipped workspace_id=ws-e3bcae071 container_id=9b4bc0fb0fde9b9dadff220fedc7360c183b86b716cb08cf8246563218fc0b58 reason=plugin recover: Error: container 9b4bc0fb0fde9b9dadff220fedc7360c183b86b716cb08cf8246563218fc0b58 not found: error: no such object: 9b4bc0fb0fde9b9dadff220fedc7360c183b86b716cb08cf8246563218fc0b58
 at recover (<eval>:64:13(26))
2026-11-07T19:13:09-07:00 [INFO] workspace_recovery_complete
2026-11-07T19:13:09-07:00 [WARN] unrecovered_workspaces_found count=2
2026-11-07T19:13:09-07:00 [WARN] unrecovered_workspace workspace_id=ws-b01b61037 container_name=wsd-ws-b01b61037 session_id=751440aea9a6 created_at=2026-11-07T19:01:35-07:00 repo_url=
2026-11-07T19:13:09-07:00 [WARN] unrecovered_workspace workspace_id=ws-e3bcae071 container_name=wsd-ws-e3bcae071 session_id=751440aea9a6 created_at=2026-11-07T18:59:18-07:00 repo_url=
2026-11-07T19:13:09-07:00 [WARN] unrecovered_workspaces_hint detail=these workspaces may be missing required metadata for recovery; run 'mcwsd cleanup --orphans' to remove them
2026-11-07T19:13:09-07:00 [INFO] startup_prune_complete removed=0 failed=0
2026-11-07T19:13:09-07:00 [INFO] server_starting address=127.0.0.1 port=8080 auth_token_set=true
```
