# dev-lease over MCP

`dev-lease mcp` is a Model Context Protocol server over stdio: it answers
the same questions the dashboard shows (which stacks are managed, their
state, holders, disk use) and forwards stop and remove requests to the
serve process, so every action still runs through the engine's own rules
(leases, keep labels, never-remove). The agent sees stacks and acts on
them without shelling out.

Register it with Claude Code:

```bash
claude mcp add dev-lease -- sh -c 'docker exec -i "$(docker ps -qf label=dev-lease.role=server)" dev-lease mcp'
```

Any other MCP client that starts stdio servers takes the same command, for
example in a JSON config:

```json
{
  "mcpServers": {
    "dev-lease": {
      "command": "sh",
      "args": ["-c", "docker exec -i \"$(docker ps -qf label=dev-lease.role=server)\" dev-lease mcp"]
    }
  }
}
```

`docker exec -i` keeps the stdio session open while the client talks to
it. If serve runs with a non-default `--state` (or `DEV_LEASE_STATE`), give
`dev-lease mcp` the same `--state`: it reads the managed patterns from that
file and finds serve's control socket next to it. The command runs inside the serve container (the one carrying
`dev-lease.role=server`), so the state file, the Docker socket and the
control socket are all in the right places; stop and remove go through
the control socket next to the state file, into the same runner that runs
the sweeps.

The server exposes four tools:

- `list_stacks` - the managed stacks and their state: live (holder names
  and when the lease ends), stopping, stopped (when the removal runs),
  removing, new. Read-only.
- `stack_details(name, log_service, log_lines)` - one stack: containers
  with memory use, volumes with sizes, images a removal would delete,
  the start command, and the recent logs of one service. Read-only.
- `stop_stack(name)` - stop the stack now. Nothing is deleted; the text
  result includes the command that starts it again.
- `remove_stack(name, confirm)` - remove the stack: containers, networks,
  volumes and images. This deletes data for real, so `confirm` must be
  exactly the stack name. A stack marked never remove is always refused.

Ask the agent what it sees before it removes anything: the removal is
permanent and there is no undo.
