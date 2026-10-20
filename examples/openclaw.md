# OpenClaw client setup

OpenClaw is optional. See the upstream
[client documentation](https://docs.openclaw.ai/tools/mcp),
[registry CLI](https://docs.openclaw.ai/cli/mcp/registry), and
[HTTP headers configuration](https://docs.openclaw.ai/cli/mcp/transports)
for client configuration details. The bridge is tested with the official Go MCP
client; live OpenClaw Gateway registration remains unverified.

Start the bridge with a securely provisioned `WORKSPACE_BRIDGE_TOKEN` environment
variable. The Gateway process must have that same variable available, including
when it is launched by a service manager. Do not put the literal token into a
command argument, repository, chat message, or shell history.

Merge this entry into administrator-owned OpenClaw configuration:

```json
{
  "mcp": {
    "servers": {
      "workspace-bridge": {
        "url": "http://127.0.0.1:8765/mcp",
        "transport": "streamable-http",
        "enabled": true,
        "headers": {
          "Authorization": "Bearer ${WORKSPACE_BRIDGE_TOKEN}"
        },
        "connectionTimeoutMs": 5000,
        "requestTimeoutMs": 20000,
        "toolFilter": {
          "include": ["workspace_*"]
        }
      }
    }
  }
}
```

The `${WORKSPACE_BRIDGE_TOKEN}` placeholder is resolved by OpenClaw, not by the
bridge. Keep it literal in the config file. This is static bearer authentication;
do not enable `auth: "oauth"` (which would override the header).

Then verify:

```sh
openclaw mcp doctor workspace-bridge --probe
openclaw mcp probe workspace-bridge --json
```

Use OpenClaw's agent tool policies to restrict these tools to the intended agent.
Have the consumer retain an independent event cursor per workspace, poll
`workspace_changes`, and compare the returned revision IDs using `workspace_diff`.
On `CURSOR_EXPIRED`, explicitly resynchronize with `workspace_snapshot` and
`workspace_files`; never assume missing history meant no change. Save the latest
cursor only after successfully processing the page.

If the Gateway is in a VM/container, its loopback address is different. Run the
bridge in the same network namespace with the authorized repository mounted, or
use a private authenticated tunnel terminating on host loopback. The daemon
rejects non-loopback binding. A proxy must preserve the configured Host value;
TLS/authentication and tunnel access belong to the administrator's trust boundary.
Do not expose port 8765 publicly.

Neither MCP connections nor workspace events trigger an agent turn by themselves.
Configure scheduling and notifications separately in the client. The bridge
does not register itself with clients or send notifications.
