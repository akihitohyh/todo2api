**本项目赞助商：[汪汪中转站](https://api.hyhawang.com/)：1 元 1 刀，GPT 低至 0.08x。**

# AT Maker

AT Maker 提供 CLI 和 WebUI，用于执行 OpenAI 账号注册流程，保存 ChatGPT access token，并尝试获取平台 OAuth refresh token。

> [!CAUTION]
> **使用、克隆、修改或分发本项目，即表示你已阅读并接受 [`NOTICE`](NOTICE) 的全部条款。**  
> 本项目按 **现状（AS IS）** 提供，不附任何担保；作者和维护者不承担任何责任。  
> **仅允许**用于你拥有的账号与环境、合法 CTF、明确授权且在范围内的安全研究、教学和离线源码研究。  
> **明确禁止：**欺诈、批量造号转售、针对未授权目标的自动化、故意滥用平台或规避服务条款。  
> 一切法律责任和使用后果由使用者独立承担。不接受这些条款时，请勿使用、请勿克隆，并删除所有副本。

---

## 法律边界

| | |
| --- | --- |
| **允许** | 你自己的账号和环境；明确授权的安全研究；CTF、学术协议研究与教学；离线阅读源码 |
| **禁止** | 欺诈、批量注册转售、账号黑产、对未授权目标的自动化、故意规避或滥用平台服务条款 |
| **责任** | 账号封禁、额度损失、数据泄露、民事、刑事或行政后果均由使用者承担 |
| **关联** | 本项目不隶属于 OpenAI / ChatGPT、Microsoft / Outlook、Cloudflare、TempMail.lol、YYDS Mail、chatgpt2api 上游或其他邮箱、验证码及代理服务商；上方披露的赞助关系不代表任何第三方平台认可或背书 |

完整条款见 [`NOTICE`](NOTICE)。本项目采用 [MIT License](LICENSE)，但 **MIT License 并非完整免责声明**。

如果你无法确定用途是否合法，请不要运行；请先咨询执业律师，或联系目标平台的安全与合规团队。

---

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
- Automatic account failover, cooldowns, and persistent disabling of exhausted keys
- Encrypted SQLite API key storage and an embedded management WebUI
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
# Generate once, then paste the output into storage.master_key in config.yaml.
openssl rand -base64 32
go run ./cmd/todo2api -config config.yaml
```

Open `http://localhost:8080` and sign in with `web.admin_username` and
`web.admin_password`. The WebUI manages API keys, health checks, balances, and
local gateway usage statistics. The account page can also bulk-import keys by
pasting one key per line or selecting a TXT file; blank lines, comments, and
duplicates are ignored. Registration automation is intentionally disabled.

## Build for Linux

The build script installs the locked WebUI dependencies, builds the embedded
frontend, runs the Go test suite, and produces a static Linux binary plus its
SHA-256 checksum:

```bash
./build.sh
./build.sh --arch arm64
```

The default output is `build/todo2api-linux-<arch>`. Run `./build.sh --help` for
custom output paths and options to reuse an existing WebUI build or skip tests.

`storage.master_key` must contain the Base64 encoding of exactly 32 bytes and
must remain stable. It is used to derive the AES-256-GCM key that encrypts
upstream API keys in SQLite; a missing or different key makes startup fail
instead of silently losing access to credentials. Protect `config.yaml` with
mode `0600`. Relative `storage.path` values resolve from the configuration file
directory.
When deploying behind a reverse proxy, list its source CIDRs in
`web.trusted_proxies`. Forwarded headers are ignored for every other peer; the
trusted proxy must overwrite client-supplied forwarded headers.

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

On the first start with an empty database, legacy account pools can be imported
from YAML and key files. Each file contains one API key per line; blank lines,
comments, and duplicates are ignored:

```yaml
pool:
  strategy: round_robin
  key_files:
    - /etc/todo2api/accounts.keys
  keys: []
```

The import is transactional and permanently marked in SQLite. After it
succeeds, YAML and key files are retained for rollback but are never read
again, even if every database account is later deleted. Add, disable, restore,
and permanently delete keys through the WebUI or management API.

The first configured accounts are initialized synchronously so the gateway can
start in seconds. Remaining accounts are initialized in small background
batches while retaining stable database IDs for pool reconciliation. Temporary
initialization failures are retried and exposed in the WebUI. The service can
start with an empty or temporarily unusable account pool; `/v1` returns `503`
until a usable key is available.

For new conversations, the gateway retries another available account after a
recognized account failure. HTTP `429` temporarily cools an account down, while
HTTP `402` or an explicit insufficient-balance/subscription-required response
persistently disables the account as exhausted while preserving its encrypted
credential and diagnostics for later recovery. If no account can accept a new
conversation, the gateway returns HTTP `503` with `Retry-After: 60`.

Account balance and subscription health are refreshed at startup, every five
minutes, or on demand from the WebUI. SQLite remains the only live credential
source after the one-time import.

Basic request:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-todo2api-changeme' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"Hello"}]}'
```

批量导入 API Key（管理会话登录后）：

```bash
curl http://localhost:8080/api/accounts/bulk \
  -H 'Content-Type: application/json' \
  -H 'Origin: http://localhost:8080' \
  -b 'todo2api-admin=<session-cookie>' \
  -d '{"keys":"key-one\nkey-two\n# comment"}'
```

也可以把同样的内容作为 `multipart/form-data` 的 `file` 字段上传；接口会
忽略空行、整行注释和重复 Key，并只返回脱敏结果统计。

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

Responses `input_image` parts are accepted in message content. `data:image/...`
URLs are decoded and uploaded to todofor.ai's attachment endpoint, then sent as
real todo attachments so vision-capable models can inspect the image bytes.
Anthropic `/v1/messages` `image` blocks with a base64 source use the same path.
Remote `http://`/`https://` URLs are preserved as references but are not fetched
by this bridge; use a data URL for reliable image recognition. `file_id` image
inputs require an upstream attachment lookup and currently return a clear 400
error.

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
account-pool selection, exhausted-account failover, encrypted credential
migration, and management authentication. Live account verification still requires
a valid todofor.ai API key; use
`examples/tool_call_curl.sh` as the account-level probe.

## Remaining work

1. Persist session references across restarts and add authenticated resume tokens.
2. Add broader compatibility for multimodal OpenAI message content.

## 致谢

<https://linux.do>
