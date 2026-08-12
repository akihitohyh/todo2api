# todo2api

OpenAI-compatible API gateway for [todofor.ai](https://todofor.ai), inspired by
[grok2api](https://github.com/chenyme/grok2api).

It pools multiple todofor.ai API keys behind OpenAI Chat Completions,
OpenAI Responses, and Anthropic Messages compatible endpoints. It translates
each protocol into todo turns and returns client-side tool calls without
allowing the upstream agent to execute local device tools itself.

## Current status

- OpenAI-compatible non-streaming responses and incremental SSE responses
- OpenAI `/v1/responses` typed items, function/custom/namespace tools, and typed SSE
- Anthropic `/v1/messages` content blocks, tool use/results, and SSE
- Claude Code-compatible top-level `system`, cache-control, thinking, and tool history handling
- Dynamically discovered `/v1/models` catalog and optional gateway bearer-token authentication
- Round-robin and least-busy account selection
- Automatic account failover, cooldowns, and persistent removal of exhausted keys
- Correct todofor.ai frontend WebSocket subscription flow
- Client-side tool calls with `finish_reason: "tool_calls"`
- In-memory continuation by canonical history hash
- `todoId` continuation fallback through response metadata or HTTP headers
- Optional Edge MCP discovery and `filteredEdgeTools` forwarding
- Exact per-turn token usage from upstream assistant `runMeta`

For streaming requests, each upstream `block:message` fragment is flushed to
the downstream connection as it arrives. Client-side tool protocol blocks are
withheld until their closing tag is available so they can be returned as one
valid structured tool call instead of leaking protocol text.

## Upstream mapping

| OpenAI | todofor.ai |
| --- | --- |
| `POST /v1/chat/completions` | `POST /projects/{projectId}/todos` (include `todoId` to resume) |
| `POST /v1/responses` | Responses Items converted to todo turns |
| `POST /v1/messages` | Anthropic content blocks converted to todo turns |
| `messages[]` | Flattened todo content or a follow-up tool result |
| `model` | `agentSettings.model`, after alias resolution |
| `tools[]` | Strict `<TOOL_CALL>` system protocol |
| `stream: true` | Incremental SSE from upstream WebSocket blocks |
| Gateway bearer token | Never forwarded upstream |

Upstream requests use `X-API-Key`. The frontend event flow is:

1. Connect `wss://<host>/ws/v1/frontend?tabId=<uuid>` with the API key as the
   WebSocket subprotocol.
2. Create or resume the todo.
3. `POST /todos/{todoId}/subscribe` with `X-API-Key`, `X-Tab-ID`, and
   `{"todoId":"..."}`.
4. Forward `block:message` payloads immediately and finish on terminal
   `todo:status` values such as `READY`, `READY_CHECKED`, or `DONE`.
5. Read `GET /todos/{todoId}/messages` for the authoritative final assistant
   message.
6. Map its AI `runMeta` counters into the requested API's usage schema.

This was checked against the current public
[OpenAPI document](https://api.todofor.ai/openapi.json) and the official
[CLI](https://github.com/todoforai/cli) /
[frontend WebSocket client](https://github.com/todoforai/edge/blob/main/bun/src/frontend-ws.ts).

## Run

```bash
cp config.example.yaml config.yaml
export TODOFOR_API_KEY='your-todofor-api-key'
go run ./cmd/todo2api -config config.yaml
```

For each account, startup loads the configured `agent_id` as the complete
`AgentSettings` template, or selects the account's first saved agent when the
field is empty. Per request, the gateway only overrides the resolved model and,
when client tools are present, the raw tool protocol and its permissions.

Configured upstream model values use todofor.ai's
`provider:author/model_id` format, such as
`openai:openai/gpt-5.6-sol`. Clients normally use the automatically generated
short model names.

At startup, the gateway queries the same `GET /api/v1/models` endpoint used by
the official todofor.ai CLI for every configured account. `/v1/models` exposes
short IDs from the intersection, so every advertised model is available
regardless of which pool account is selected. Entries include the upstream
owner, creation time, context window, and maximum completion tokens. For
example:

```text
claude-sonnet-4.6
gemini-2.5-flash
gpt-5.6-sol
grok-4.20
```

Full provider/model IDs and runner IDs remain accepted as compatibility input
and are converted to the runner's provider-qualified form on use, such as
`anthropic:anthropic/claude-sonnet-4.6`. If providers advertise the same short
ID, those entries keep their provider prefix to remain unambiguous. Explicit
aliases under `models.aliases` override discovered IDs on collision. A
transient catalog failure is logged as a startup warning and falls back to the
configured aliases and short default name instead of preventing startup.

Large account pools can be loaded from files without expanding the YAML or
systemd environment. Each file contains one API key per line; blank lines,
comments, and duplicates are ignored:

```yaml
pool:
  strategy: round_robin
  key_files:
    - /etc/todo2api/accounts.keys
  keys: []
```

The first configured accounts are initialized synchronously so the gateway can
start in seconds. Remaining accounts are initialized in small background
batches while retaining file order for round-robin selection. Temporary
initialization failures are retried; accounts that still cannot load a project
or Agent template are skipped. Startup fails only when none are usable.

For new conversations, the gateway retries another available account after a
recognized account failure. HTTP `429` temporarily cools an account down, while
HTTP `402` or an explicit insufficient-balance/subscription-required response
permanently disables the account and removes its key from both inline
`pool.keys` entries and configured `pool.key_files`. Credential files are
rewritten with `fsync` and an atomic rename, so removed keys do not return after
a configuration reload or service restart. If no account can accept a new
conversation, the gateway returns HTTP `503` with `Retry-After: 60`.

When `pool.key_files` is set, those files are polled every 2s (500ms debounce).
On content change the gateway re-reads the config source plus key files, merges
them, and hot-updates the account pool:

- **added** keys are initialized in the background and enter rotation
- **removed** keys are soft-deleted: excluded from new `Pick` traffic, but keep
  their account index so in-flight multi-turn sessions can finish
- **reappearing** keys are restored in place (same index) when possible

Atomic replace of a key file is safe: a brief missing/unreadable file skips that
tick and keeps the last good set. Restart is not required for key file edits.
Hot reload also respects permanent key removals written by account failover.

Basic request:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-todo2api-changeme' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"Hello"}]}'
```

Incremental Chat Completions stream:

```bash
curl --no-buffer http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-todo2api-changeme' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Write a detailed answer in several paragraphs."}]}'
```

With `stream_options.include_usage`, Chat Completions emits the standard final
usage-only chunk with an empty `choices` array before `[DONE]`. Responses places
usage on the completed response event, while Anthropic Messages places it on
the final `message_delta`. `/v1/messages/count_tokens` remains an estimate
because it runs before an upstream assistant message exists.

Responses request:

```bash
curl http://localhost:8080/v1/responses \
  -H 'Authorization: Bearer sk-todo2api-changeme' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","input":"Hello"}'
```

Set `"stream":true` and use `curl --no-buffer` to receive
`response.output_text.delta` events before `response.completed`.

Anthropic Messages request:

```bash
curl http://localhost:8080/v1/messages \
  -H 'x-api-key: sk-todo2api-changeme' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'
```

Set `"stream":true` and use `curl --no-buffer` to receive Anthropic
`content_block_delta` events before `message_stop`.

Claude Code's top-level `system` field is kept separate from the Anthropic
`messages` array and forwarded as the upstream agent system prompt. For
compatibility with clients that include historical `role: "system"` entries,
the gateway merges those entries into the same system prompt instead of
rejecting them or forwarding them as ordinary conversation messages. Common
`cache_control`, `thinking`, `tool_use`, and `tool_result` blocks are accepted.

`/v1/messeges` is also registered as a compatibility alias for clients with
that misspelling. `/v1/messages/count_tokens` is available as an estimate
because todofor.ai does not expose the selected model's tokenizer.

## Client tools

When `tools` is present, the gateway uses `systemMessageMode: "raw"` and injects
a strict prompt requiring exactly one block:

```text
<TOOL_CALL>{"name":"read_file","arguments":{"path":"/tmp/example.txt"}}</TOOL_CALL>
```

It also sends these permissions by default:

```json
{
  "allow": [],
  "deny": ["device:*", "cloud:*"]
}
```

The patterns follow todofor.ai's permission matcher: `device:*` covers concrete
Edge/bridge devices, while `cloud:*` blocks the hosted cloud machine. This is a
second enforcement layer behind the raw system prompt. It prevents upstream
device execution, but no prompt can mathematically guarantee model compliance;
the gateway only converts syntactically valid `<TOOL_CALL>` blocks.

Run the complete two-request curl flow, including local file execution:

```bash
./examples/tool_call_curl.sh
```

The client should repeat `tools` on every tool-result request, as standard
OpenAI clients do.

Responses dynamic tool definitions support `function`, `custom`, and
`namespace`. Namespace children are qualified only while talking to the
upstream agent, then restored as Responses `function_call` items with separate
`name` and `namespace` fields. Server-executed Responses tools such as
`web_search`, `code_interpreter`, and `mcp` are not emulated by the gateway.

## Continuation

The gateway hashes the complete canonical message history, including assistant
`tool_calls`. If a client trims or reorders history, return either extension on
the next request:

```json
{
  "metadata": {
    "todo2api.todo_id": "the-id-from-the-previous-response"
  }
}
```

Or use the response/request header:

```text
X-Todo2API-Todo-ID: <todo-id>
```

The fallback uses an in-memory reverse index with a 30-minute expiry and
therefore does not survive a gateway restart. Persisting and signing
conversation references is a future hardening step.

## Test

```bash
go test ./...
go build ./cmd/todo2api
```

Tests include an HTTP/WebSocket mock of the official subscription protocol,
pre-terminal delta timing, split tool-tag filtering, cancellation, tool-call
continuation, Responses Item/SSE conversion, Anthropic content/SSE conversion,
Claude Code system-message conversion, exact `runMeta` usage mapping,
account-pool selection, exhausted-account failover, and persistent credential
removal across configuration reloads. Live account verification still requires
a valid todofor.ai API key; use
`examples/tool_call_curl.sh` as the account-level probe.

## Remaining work

1. Persist session references across restarts and add authenticated resume tokens.
2. Add broader compatibility for multimodal OpenAI message content.
