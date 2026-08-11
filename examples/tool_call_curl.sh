#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
CLIENT_TOKEN="${CLIENT_TOKEN:-sk-todo2api-changeme}"
MODEL="${MODEL:-gpt-5.6-sol}"
DEMO_FILE="${DEMO_FILE:-/tmp/todo2api-demo.txt}"

command -v curl >/dev/null
command -v jq >/dev/null

if [[ ! -f "$DEMO_FILE" ]]; then
  printf 'hello from the local tool client\n' >"$DEMO_FILE"
fi

tools_json=$(jq -n '
  [{
    type: "function",
    function: {
      name: "read_file",
      description: "Read a UTF-8 text file from the client machine",
      parameters: {
        type: "object",
        properties: {path: {type: "string"}},
        required: ["path"],
        additionalProperties: false
      }
    }
  }]')

prompt="Use read_file to read ${DEMO_FILE}, then tell me exactly what it contains."
first_body=$(jq -n \
  --arg model "$MODEL" \
  --arg prompt "$prompt" \
  --argjson tools "$tools_json" \
  '{model: $model, messages: [{role: "user", content: $prompt}], tools: $tools}')

first=$(curl -fsS "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "$first_body")

printf '%s\n' "$first" | jq .

todo_id=$(jq -er '.metadata["todo2api.todo_id"]' <<<"$first")
assistant=$(jq -c '.choices[0].message' <<<"$first")
call_id=$(jq -er '.choices[0].message.tool_calls[0].id' <<<"$first")
tool_name=$(jq -er '.choices[0].message.tool_calls[0].function.name' <<<"$first")
tool_path=$(jq -er '.choices[0].message.tool_calls[0].function.arguments | fromjson | .path' <<<"$first")

if [[ "$tool_name" != "read_file" ]]; then
  printf 'unsupported demo tool: %s\n' "$tool_name" >&2
  exit 1
fi
tool_result=$(<"$tool_path")

second_body=$(jq -n \
  --arg model "$MODEL" \
  --arg prompt "$prompt" \
  --argjson assistant "$assistant" \
  --arg call_id "$call_id" \
  --arg tool_name "$tool_name" \
  --arg tool_result "$tool_result" \
  --arg todo_id "$todo_id" \
  --argjson tools "$tools_json" \
  '{
    model: $model,
    messages: [
      {role: "user", content: $prompt},
      $assistant,
      {role: "tool", tool_call_id: $call_id, name: $tool_name, content: $tool_result}
    ],
    tools: $tools,
    metadata: {"todo2api.todo_id": $todo_id}
  }')

second=$(curl -fsS "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Todo2API-Todo-ID: $todo_id" \
  -d "$second_body")

printf '%s\n' "$second" | jq .
