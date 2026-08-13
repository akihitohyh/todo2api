package session

import (
	"context"
	"sync"
	"time"
)

// Store maps a conversation key to the upstream todo it lives in, plus the
// account index that owns it (so continuations reuse the same key).
type Store struct {
	mu        sync.RWMutex
	byHistory map[string]Entry
	byTodoID  map[string]Entry
	toolNames map[string]map[string]string
}

type Entry struct {
	TodoID    string
	Account   int
	ExpiresAt time.Time
}

func New() *Store {
	return &Store{
		byHistory: map[string]Entry{},
		byTodoID:  map[string]Entry{},
		toolNames: map[string]map[string]string{},
	}
}

func (s *Store) PutToolNames(todoID string, names map[string]string) {
	if todoID == "" || len(names) == 0 {
		return
	}
	copyNames := make(map[string]string, len(names))
	for callID, name := range names {
		copyNames[callID] = name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolNames[todoID] = copyNames
}

func (s *Store) ToolName(todoID, callID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name, ok := s.toolNames[todoID][callID]
	return name, ok
}

func (s *Store) Get(key string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byHistory[key]
	return e, ok
}

func (s *Store) GetByTodoID(todoID string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byTodoID[todoID]
	return e, ok
}

func (s *Store) Put(key string, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		s.byHistory[key] = e
	}
	if e.TodoID != "" {
		s.byTodoID[e.TodoID] = e
	}
}

// StartCleanup periodically removes expired session state.
func (s *Store) StartCleanup(interval time.Duration) {
	s.StartCleanupContext(context.Background(), interval)
}

func (s *Store) StartCleanupContext(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanup()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Store) cleanup() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entry := range s.byHistory {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(s.byHistory, key)
		}
	}

	validTodoIDs := make(map[string]struct{})
	for todoID, entry := range s.byTodoID {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(s.byTodoID, todoID)
		} else {
			validTodoIDs[todoID] = struct{}{}
		}
	}

	for todoID := range s.toolNames {
		if _, exists := validTodoIDs[todoID]; !exists {
			delete(s.toolNames, todoID)
		}
	}
}
