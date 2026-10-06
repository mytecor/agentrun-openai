# agentrun-openai

An OpenAI-compatible HTTP gateway over [`github.com/dmora/agentrun`](https://github.com/dmora/agentrun). It exposes ACP agents as models and maps client-provided OpenAI function tools to a session-scoped MCP server. Agents can also use their own native tools, subprocesses, and subagents.

## Endpoints

- `GET /healthz`
- `GET /v1/models` (also `GET /models`)
- `POST /v1/chat/completions`, with `stream` either way

Model IDs:

- `<backend-id>` leaves the model choice to the backend's own default and does not advertise or accept `reasoning_effort`; select a concrete model to choose effort.
- `<backend-id>/<model-id>` selects one explicitly.

Concrete models for configured ACP backends are discovered from agentrun's model-catalog API on every `/models` request; if discovery fails the last known catalog is kept, and the backend-default IDs always work. By default, effort variants (e.g. `gpt-5[low]`, `gpt-5[medium]`) collapse into one entry per base model — pick the level through the OpenAI `reasoning_effort` field using an exact value from `reasoning_efforts`; the selected bracketed model ID is sent to ACP. If omitted, the first variant in the backend catalog is selected.

## Install

Requirements: at least one ACP-compliant agent or adapter (such as agents from the [ACP Registry](https://github.com/agentclientprotocol/registry)).

Download the archive for your platform from the [releases page](https://github.com/mytecor/agentrun-openai/releases) and put the binary on `PATH`. Builds cover Linux, macOS, and Windows on `amd64` and `arm64`, and every release carries `SHA256SUMS`. The binaries are unsigned, so a macOS download through a browser needs `xattr -d com.apple.quarantine` before the first run.

### Build from source

Needs Go 1.24+. The module pins its agentrun dependency with a `replace` directive, which `go install <module>@latest` rejects, so clone first:

```sh
git clone https://github.com/mytecor/agentrun-openai
cd agentrun-openai
GOBIN="$HOME/.local/bin" go install ./cmd/agentrun-openai
```

The server runs as the current user, so the agent CLIs must be on that user's `PATH` and authenticated for them.

## Run

```sh
agentrun-openai \
  --acp codex="npx @agentclientprotocol/codex-acp" \
  --acp claude="npx @agentclientprotocol/claude-agent-acp"
```

The default is `127.0.0.1:8787`. Options:

```text
--host 127.0.0.1
--port 8787
--api-key local-secret
--default-cwd /absolute/path/to/project
--allowed-root /absolute/path/to/projects
--acp codex="npx @agentclientprotocol/codex-acp"
--turn-timeout 30m
--session-ttl 10m
--session-store "/path/to/sessions.json"
--stream-heartbeat 20s
--shutdown-timeout 10s
--version
```

`--allowed-root` is repeatable, and equals `AGENTRUN_ALLOWED_ROOTS` using the OS path-list separator. With at least one root set, `X-Agent-CWD` must resolve inside one of them and symlink escapes are rejected; with none, any absolute path is accepted.

`--acp` is repeatable (or configured via `AGENTRUN_ACP`), registering ACP backends.


Every request may set `X-Agent-CWD` to an absolute project directory. Session affinity comes from the first of `X-Session-Affinity`, `Session-ID`, the `session_id` header, `X-Client-Request-ID`, or JSON `session_id`. With none present the gateway mints an ID and returns it as `X-Session-ID`.

## Client function calling

Send standard Chat Completions `tools` with `type: "function"`. Each function
becomes one MCP tool with the same name, description and JSON Schema parameters.
The ACP agent receives the stdio descriptor through `agentrun.Session.MCPServers`.
The gateway never executes the functions; the OpenAI client does.

For example, the first request can contain:

```json
{
  "model": "my-agent",
  "session_id": "example-session",
  "messages": [{"role": "user", "content": "Look up item 42"}],
  "tools": [{
    "type": "function",
    "function": {
      "name": "lookup_item",
      "description": "Look up an item by ID",
      "parameters": {
        "type": "object",
        "properties": {"id": {"type": "integer"}},
        "required": ["id"]
      }
    }
  }]
}
```

When the agent calls `lookup_item`, the gateway returns an assistant message with
`tool_calls` and `finish_reason: "tool_calls"`. Append that exact assistant message
and a tool result to the conversation, then submit the full history with the same
model, session affinity, working directory and `tools`:

```json
{"role": "tool", "tool_call_id": "call_...", "content": "Item 42: available"}
```

Use the call ID returned by the gateway. If no affinity was supplied initially,
copy the response's `X-Session-ID` into the next request's `session_id` field or
`X-Session-Affinity` header. A tool result releases the pending MCP request and
continues the **same ACP turn**; it never sends another `session/prompt`. Further
calls can produce more exchanges before the final assistant response with
`finish_reason: "stop"`. Concurrent MCP calls may be delivered in separate
responses. Supply a result for every call in the assistant response.

`stream: true` uses the same flow: a complete `delta.tool_calls` (including index,
ID, name and arguments), `finish_reason: "tool_calls"`, then `data: [DONE]`. The
ACP turn remains alive after that stream closes. Text before a tool call is
included in its response; subsequent output belongs to the next response.

`tool_choice` supports `auto` (default), `none`, `required`, and
`{"type":"function","function":{"name":"lookup_item"}}`. The facade rejects
calls forbidden by the current choice. Required/named choices also instruct the
agent to call a tool; if the ACP agent finishes without the required call, the
gateway returns an API error instead of a successful completion. Choice can be
updated when submitting tool results, without starting another ACP prompt.
Schema-constrained argument generation remains the ACP agent's responsibility.

Repeat `tools` on every request. Changes to function definitions recreate the
native session and facade; JSON object key order and tool list order do not.
Omitting `tools` means an empty toolset. Unknown, duplicate, stale or incomplete
tool results return a controlled 4xx error. Correctable result errors leave the
pending turn available for retry. Changing tools while submitting a result
cancels the old turn and rejects that result.

The private `__mcp-bridge` mode uses the MCP Go SDK over stdio and a separate
loopback-only IPC listener with a random token per session. It is not an HTTP
endpoint on the public gateway. No backend-specific tool definitions or dispatch
rules are used; system/developer messages do not define MCP tools.

Tool sessions stay in memory and are not resumed across gateway restarts or idle
eviction. Turn timeout includes time waiting for client tool results. Timeout,
bridge disconnect, session reset, eviction and shutdown cancel pending calls and
release their resources. Requests without tools retain the ordinary ACP behavior
and attach no MCP servers.

## ACP backends and adapters

Any ACP-compliant agent talking JSON-RPC over stdio can be registered without modifying source code using `--acp`:

```sh
# OpenAI Codex adapter with automatic effort detection
agentrun-openai \
  --acp codex="npx @agentclientprotocol/codex-acp"

# Multiple ACP agents
agentrun-openai \
  --acp codex="npx @agentclientprotocol/codex-acp" \
  --acp claude="npx @agentclientprotocol/claude-agent-acp" \
  --acp pi=pi-acp \
  --acp opencode="opencode acp"
```

Command and arguments are passed directly to `exec` without shell interpretation (`/bin/sh -c`). The agent executable must speak the Agent Client Protocol (ACP) via JSON-RPC 2.0 over `stdin`/`stdout`.

### Automatic reasoning effort detection

Adapters such as `@agentclientprotocol/codex-acp` expose effort variants as bracketed model IDs (e.g. `o3-mini[low]`, `o3-mini[medium]`, `o3-mini[high]`). The gateway automatically groups these into one base model in `/v1/models` with `reasoning_efforts`, regardless of the backend name. Any nonempty bracketed effort value is accepted, preserving its spelling and case. Models without a valid bracket suffix remain unchanged.

In chat requests, `"reasoning_effort": "high"` selects the corresponding bracketed model variant. No additional configuration is needed. Values and per-model order come from the catalog; `thinking_level_map` contains only those values, without aliases. A default effort is not advertised because the catalog API does not provide one. Requests with an explicit effort for a model without advertised effort variants are rejected.

### Model namespace

Each generic ACP backend automatically provides an OpenAI model namespace:

```text
pi
pi/<model>
```

For example, if `pi-acp` publishes models `gpt-5.6-sol` and `claude-sonnet-4.6`, `/v1/models` returns:

```text
pi
pi/gpt-5.6-sol
pi/claude-sonnet-4.6
```

### OpenAI request

Using the base model ID lets the ACP backend choose its default model:

```json
{
  "model": "pi",
  "messages": [
    {"role": "user", "content": "hello"}
  ]
}
```

Using a submodel ID specifies the model to the ACP backend:

```json
{
  "model": "pi/gpt-5.6-sol",
  "messages": [
    {"role": "user", "content": "hello"}
  ]
}
```

All existing features — session affinity, `--session-ttl`, idle eviction, native ACP session resume, transcript fingerprinting, `X-Agent-CWD`, streaming, and heartbeat — work identically for all generic ACP backends.

## Streaming and long tool runs

A turn can spend minutes inside the agent's own tools without emitting text, so the gateway keeps it visible two ways:

- Thinking is streamed as `reasoning_content` deltas, separate from `content`, and never enters the stored transcript. Agents stream their thought text according to ACP protocol events.
- After `--stream-heartbeat` of silence (20s) a keep-alive delta is sent. It carries a zero-width space, because OpenAI clients skip a chunk whose delta is empty and would still time out — Pi's watchdog aborts at 90s. The marker renders as nothing and stays out of both the answer and the stored reasoning. A negative duration disables it; `AGENTRUN_STREAM_HEARTBEAT` sets it too.

Tool calls never cross the HTTP boundary as OpenAI `tool_calls`. The backend has already run them, and a client that received them would run them a second time.

## Pi configuration

Install [`pi-models-discovery`](https://www.npmjs.com/package/pi-models-discovery) once:

```sh
pi install npm:pi-models-discovery
```

Then mark the provider for discovery in `~/.pi/agent/models.json`. Pi reads every served model from `GET /v1/models`, so no handwritten `models` array is needed.

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
        "sessionAffinityFormat": "openai",
        "supportsReasoningEffort": true
      }
    }
  }
}
```

Add `"agentrun/**"` to `enabledModels` in `~/.pi/agent/settings.json` to make every discovered model selectable; the double glob also matches nested IDs such as `agentrun/codex/gpt-5.6-sol`. Run `/config:model-discovery-refresh` in Pi after the server's model list changes.

Pi's thinking-level selector arrives as `reasoning_effort`, and changing it starts a separate native session so a conversation never silently keeps the old effort. To replace a generic `thinkingLevelMap` from the extension, use provider-level `modelOverrides` in `models.json` rather than editing anything under `node_modules`.

## Sessions

An idle agent process is kept for 10 minutes. On Windows shutting one down is blunter than elsewhere: the platform has no SIGTERM, so a backend that does not exit when its input closes is killed once the grace period ends. The gateway stores the backend's native resume ID with the working directory, message count, and a SHA-256 transcript fingerprint — never message text. After idle eviction or a restart, a matching history resumes the native session and sends only the new turn; if that session is gone, it retries once with the full conversation. A diverged history or a changed working directory discards the resume ID and starts a fresh agent with the supplied branch as context.

Because an OpenAI HTTP client cannot answer interactive permission prompts, sessions run with agentrun's HITL mode disabled, so agents use their native tools freely within `X-Agent-CWD`. Keep the server on localhost or set `--api-key` before exposing it.

## Building and releasing

`ci.yml` runs `gofmt`, `go vet`, `go test`, and a cross-compile of every released platform on each push to `main` and each pull request.

`release.yml` never starts on its own — no push, tag, or schedule trigger, only a manual run from the Actions tab or the CLI:

```sh
gh workflow run release.yml -f bump=patch
```

**Neither the version nor the tag is written by hand.** The run raises the highest existing `vX.Y.Z` tag by `bump` (`patch`, `minor`, `major`), starting at `v0.1.0` in a repository with no tags, and prints the result in the log and run summary before anything is published. Pre-release tags never seed a bump, so release candidates and jumps need an explicit version, which overrides the bump:

```sh
gh workflow run release.yml -f version=v1.0.0-rc.1
```

The run tests, builds, and only then tags the checked-out commit and publishes a release with generated notes, every archive, and `SHA256SUMS` — so a failed build leaves no tag behind. An explicit version that is not `vX.Y.Z` (an optional `-rc.1` suffix is fine), or one whose tag exists, fails before anything is built. Add `-f dry_run=true` to build the archives as a workflow artifact without tagging or publishing.

Both workflows call `scripts/build-release.sh`, which also runs locally and writes to `dist/`:

```sh
scripts/build-release.sh v0.1.0
```

The argument is stamped in through `-ldflags -X main.version=...` and reported by `agentrun-openai --version`; with no argument the script falls back to `git describe`.
