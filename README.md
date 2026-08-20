# agentrun-openapi

An OpenAI-compatible HTTP gateway over [`github.com/dmora/agentrun`](https://github.com/dmora/agentrun). It exposes complete coding agents as models while keeping their tools, subprocesses, and subagents inside the agent runtime.

## Endpoints

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/chat/completions` with `stream: false` or `stream: true`

Built-in models:

- `claude-code` uses agentrun's persistent Claude Code streaming backend.
- `codex` uses agentrun's persistent ACP backend and a `codex-acp` executable.

## Install

Requirements: Go 1.24+, an authenticated `claude` CLI, and/or a globally installed and authenticated `codex-acp` executable on `PATH`.

### Install for the current user

From the repository root, install the executable into a directory already present on `PATH`:

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/agentrun-openapi
agentrun-openapi --help
```

Create `$HOME/.local/bin` and add it to `PATH` first if your shell does not already use it.

### Install system-wide

Build a release binary and install it into `/usr/local/bin`:

```sh
go build -trimpath -o agentrun-openapi ./cmd/agentrun-openapi
sudo install -m 0755 agentrun-openapi /usr/local/bin/agentrun-openapi
agentrun-openapi --help
```

To update an existing installation, pull the new project version and repeat the same build and `install` commands. The server runs as the current user, so `claude` and `codex-acp` must be available on that user's `PATH` and authenticated for that user.

## Run

```sh
agentrun-openapi
```

To select another port or listen interface:

```sh
agentrun-openapi --host 127.0.0.1 --port 9000
```

The server listens on `127.0.0.1:8787` by default. Useful options:

```text
--host 127.0.0.1
--port 8787
--api-key local-secret
--default-cwd /absolute/path/to/project
--claude-binary claude
--codex-acp-binary codex-acp
--codex-acp-args arg1,arg2
--turn-timeout 30m
--session-ttl 1h
```

Every request may set `X-Agent-CWD` to an absolute project directory. Session affinity is resolved in this order:

1. `X-Session-Affinity`
2. `Session-ID`
3. `session_id` (header)
4. `X-Client-Request-ID`
5. JSON `session_id`

If none is present, the gateway creates a one-off ID and returns it as `X-Session-ID`.

## Pi configuration

```json
{
  "providers": {
    "agentrun": {
      "baseUrl": "http://127.0.0.1:8787/v1",
      "api": "openai-completions",
      "apiKey": "local",
      "headers": {
        "X-Agent-CWD": "!pwd"
      },
      "compat": {
        "sendSessionAffinityHeaders": true,
        "sessionAffinityFormat": "openai"
      },
      "models": [
        {"id": "claude-code", "name": "Claude Code", "input": ["text"], "contextWindow": 200000, "maxTokens": 32000},
        {"id": "codex", "name": "Codex", "input": ["text"], "contextWindow": 200000, "maxTokens": 32000}
      ]
    }
  }
}
```

The gateway stores the transcript it expects for each `(session affinity, model)` pair. A linear request sends only the newly appended user turn through `agentrun.RunTurn`. If the incoming history diverges or the working directory changes, the old process is stopped and a fresh agent is started with the supplied branch as context.

Internal agent tool calls are deliberately not exposed as OpenAI `tool_calls`; only assistant text crosses the HTTP boundary.

Because an OpenAI HTTP client cannot answer Claude/Codex interactive permission prompts, sessions run with agentrun's HITL mode disabled. Agents can therefore use their native tools within `X-Agent-CWD`; keep the server bound to localhost or configure `--api-key` before exposing it to a network.
