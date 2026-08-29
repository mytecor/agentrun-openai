# agentrun-openai

An OpenAI-compatible HTTP gateway over [`github.com/dmora/agentrun`](https://github.com/dmora/agentrun). It exposes complete coding agents as models, keeping their tools, subprocesses, and subagents inside the agent runtime.

## Endpoints

- `GET /healthz`
- `GET /v1/models` (also `GET /models`)
- `POST /v1/chat/completions`, with `stream` either way

Model IDs:

- `claude-code`, `codex`, and `agy` leave the model choice to the backend's own default.
- `claude-code/<model-id>` and `codex/<model-id>` select one explicitly.

`agy` runs the Antigravity CLI, which advertises no model catalog, so it stays a single entry with no sub-models. Concrete models for the other two are discovered from agentrun's model-catalog API on every `/models` request; if discovery fails the last known catalog is kept, and the backend-default IDs always work. Codex effort variants collapse into one entry per base model — pick the level through the OpenAI `reasoning_effort` field (`low`, `medium`, `high`, `xhigh`, `max`; default `medium`), which is sent separately from the model.

## Install

Requirements: at least one authenticated agent CLI on `PATH` — `claude`, `codex-acp`, or Antigravity's `agy`.

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
agentrun-openai --host 127.0.0.1 --port 9000
```

The default is `127.0.0.1:8787`. Options:

```text
--host 127.0.0.1
--port 8787
--api-key local-secret
--default-cwd /absolute/path/to/project
--allowed-root /absolute/path/to/projects
--claude-binary claude
--codex-acp-binary codex-acp
--agy-binary agy
--codex-acp-args arg1,arg2
--turn-timeout 30m
--session-ttl 10m
--session-store "/path/to/sessions.json"
--stream-heartbeat 20s
--claude-thinking-budget 0
--shutdown-timeout 10s
--version
```

`--allowed-root` is repeatable, and equals `AGENTRUN_ALLOWED_ROOTS` using the OS path-list separator. With at least one root set, `X-Agent-CWD` must resolve inside one of them and symlink escapes are rejected; with none, any absolute path is accepted.

Every request may set `X-Agent-CWD` to an absolute project directory. Session affinity comes from the first of `X-Session-Affinity`, `Session-ID`, the `session_id` header, `X-Client-Request-ID`, or JSON `session_id`. With none present the gateway mints an ID and returns it as `X-Session-ID`.

## Streaming and long tool runs

A turn can spend minutes inside the agent's own tools without emitting text, so the gateway keeps it visible two ways:

- Thinking is streamed as `reasoning_content` deltas, separate from `content`, and never enters the stored transcript. Codex sends its thought text verbatim. Claude Code emits thinking only above `--claude-thinking-budget` zero and redacts the text anyway, so there the heartbeat alone shows life.
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
