# OpenWebUI Server Usage

**MCWSD** comes with a [tool](../tools/openwebui/mcwsd/mcwsd.py) for OpenWebUI and you should increase the timeout in the tool to accommodate VM provisioning times.

## MCP Server Usage

### Provision a Workspace

![Provision A Workspcae](oi-provision.png)

### Update Workspace

![Update Workspace](oi-update.png)

### Commit and PR

![Commit and PR](oi-commit-pr.png)

### Review and Approve PR on Forge

![Approve on Forge](oi-approve-forge.png)

### Delete Workspace

![Delete Workspace](oi-delete.png)

## OpenWebUI Client Configuration

Ensure you set the token and base URL for the OpenWebUI client to communicate with the MCWSD daemon in the `mcwsd.py` tool.  Adjust the timeout values as needed for your environment.

## OpenWebUI Client Tips

OpenTerminal is supported in OpenWebUI but AI can get confused when the workspace daemon is also present as a tool.
