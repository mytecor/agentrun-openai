# agentrun-openapi

An OpenAI-compatible HTTP gateway over [`github.com/dmora/agentrun`](https://github.com/dmora/agentrun). It exposes complete coding agents as models while keeping their tools, subprocesses, and subagents inside the agent runtime.

## Endpoints

- `GET /healthz`
- `GET /v1/models` (also available as `GET /models`)
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
--allowed-root /absolute/path/to/projects
--claude-binary claude
--codex-acp-binary codex-acp
--codex-acp-args arg1,arg2
--turn-timeout 30m
--session-ttl 10m
--session-store "/path/to/sessions.json"
```

`--allowed-root` is optional and repeatable. When at least one root is configured, `X-Agent-CWD` must resolve inside one of those directories; symlink escapes are rejected. With no allowed roots, any absolute working directory is accepted. The equivalent environment variable is `AGENTRUN_ALLOWED_ROOTS`, using the operating system's path-list separator.

Every request may set `X-Agent-CWD` to an absolute project directory. Session affinity is resolved in this order:

1. `X-Session-Affinity`
2. `Session-ID`
3. `session_id` (header)
4. `X-Client-Request-ID`
5. JSON `session_id`

If none is present, the gateway creates a one-off ID and returns it as `X-Session-ID`.

## Pi configuration

Install [`pi-models-discovery`](https://www.npmjs.com/package/pi-models-discovery) once:

```sh
pi install npm:pi-models-discovery
```

Then mark the provider for discovery in `~/.pi/agent/models.json`. The optional static entries below are only an offline fallback; online, Pi obtains every served model from `GET /v1/models`.

```json
{
  "providers": {
    "agentrun": {
      "baseUrl": "http://127.0.0.1:8787/v1",
      "api": "openai-completions",
      "apiKey": "local",
      "discoverModels": true,
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

To make every discovered agent model selectable, add `"agentrun/*"` to `enabledModels` in `~/.pi/agent/settings.json`. Run `/config:model-discovery-refresh` inside Pi after changing the server's model list.

The gateway keeps an idle Claude/Codex process for 10 minutes by default. It captures the backend's native resume ID and persists it with the working directory, message count, and a SHA-256 transcript fingerprint. Message text is not written to this store. After idle eviction or a server restart, a matching Pi history resumes the native backend session and sends only the newly appended turn. If the native session has expired or been deleted, the gateway automatically retries once with the complete supplied conversation. If the history diverges or the working directory changes, the saved resume ID is discarded and a fresh agent is started with the supplied branch as context.

Internal agent tool calls are deliberately not exposed as OpenAI `tool_calls`; only assistant text crosses the HTTP boundary.

Because an OpenAI HTTP client cannot answer Claude/Codex interactive permission prompts, sessions run with agentrun's HITL mode disabled. Agents can therefore use their native tools within `X-Agent-CWD`; keep the server bound to localhost or configure `--api-key` before exposing it to a network.
