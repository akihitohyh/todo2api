package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessageUnmarshalClearsReceiver(t *testing.T) {
	var m ChatMessage

	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "" || len(m.Parts) != 2 || m.Role != "user" {
		t.Fatalf("array decode = %#v", m)
	}

	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"plain"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "plain" || len(m.Parts) != 0 || m.Role != "assistant" {
		t.Fatalf("string decode left stale parts: %#v", m)
	}

	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"again"}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "" || len(m.Parts) != 1 || m.Role != "user" {
		t.Fatalf("array decode left stale content: %#v", m)
	}

	if err := json.Unmarshal([]byte(`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.ToolCalls) != 1 {
		t.Fatalf("tool call decode = %#v", m)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":"x"}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.ToolCalls) != 0 {
		t.Fatalf("stale tool calls survived: %#v", m)
	}

	if err := json.Unmarshal([]byte(`{"role":"user","content":null}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) != 0 || m.Content != "" {
		t.Fatalf("null content left stale state: %#v", m)
	}
}

func TestChatMessageRejectsInvalidContentParts(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no type no text", `{"content":[{"foo":"bar"}]}`, "content part 0 has no type and no text"},
		{"empty object", `{"content":[{}]}`, "content part 0 has no type and no text"},
		{"unknown type", `{"content":[{"type":"audio"}]}`, `unsupported content part type "audio"`},
		{"content not string or array", `{"content":{"type":"text"}}`, "content must be a string or an array of content parts"},
		{"text with wrong value type", `{"content":[{"type":"text","text":123}]}`, "cannot unmarshal number into Go struct field ContentPart.text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m ChatMessage
			err := json.Unmarshal([]byte(tc.body), &m)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unmarshal error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestChatMessageAcceptsMissingTypeTextPart(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"content":[{"text":"bare"}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) != 1 || m.Parts[0].Type != "" || m.Parts[0].Text != "bare" {
		t.Fatalf("parts = %#v", m.Parts)
	}
}

func TestChatMessageAcceptsCamelCaseImageUrlPart(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"content":[{"type":"image_url","imageUrl":{"url":"data:image/png;base64,AAAA"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) != 1 || len(m.Parts[0].ImageURLCompat) == 0 {
		t.Fatalf("parts = %#v", m.Parts)
	}
}

func TestChatMessageRejectsEmptyContentArray(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":[]}`), &m); err == nil || !strings.Contains(err.Error(), "content array must not be empty") {
		t.Fatalf("empty array error = %v", err)
	}
	// Absent, null, and string content remain valid; Parts stays nil.
	for _, body := range []string{
		`{"role":"user"}`,
		`{"role":"user","content":null}`,
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
		`{"role":"user","content":"plain"}`,
	} {
		var m ChatMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		if m.Parts != nil {
			t.Fatalf("body %s left non-nil parts: %#v", body, m.Parts)
		}
	}
}
