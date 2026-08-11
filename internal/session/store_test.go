package session

import "testing"

func TestStoreIndexesHistoryAndTodoID(t *testing.T) {
	store := New()
	want := Entry{TodoID: "todo-1", Account: 2}
	store.Put("history-key", want)

	if got, ok := store.Get("history-key"); !ok || got != want {
		t.Fatalf("history lookup = %#v, %v", got, ok)
	}
	if got, ok := store.GetByTodoID("todo-1"); !ok || got != want {
		t.Fatalf("todo lookup = %#v, %v", got, ok)
	}

	store.PutToolNames("todo-1", map[string]string{"call-1": "read_file"})
	if got, ok := store.ToolName("todo-1", "call-1"); !ok || got != "read_file" {
		t.Fatalf("tool name lookup = %q, %v", got, ok)
	}
}
