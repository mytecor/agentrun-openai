package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type persistedSession struct {
	ResumeID     string `json:"resume_id"`
	CWD          string `json:"cwd"`
	HistoryCount int    `json:"history_count"`
	HistoryHash  string `json:"history_hash"`
}

type persistedSessions struct {
	Version  int                         `json:"version"`
	Sessions map[string]persistedSession `json:"sessions"`
}

type sessionStore struct {
	mu      sync.Mutex
	path    string
	records map[string]persistedSession
}

func newSessionStore(path string) (*sessionStore, error) {
	s := &sessionStore{path: path, records: make(map[string]persistedSession)}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read: %w", err)
	}
	var stored persistedSessions
	if err := json.Unmarshal(data, &stored); err != nil {
		return s, fmt.Errorf("decode: %w", err)
	}
	if stored.Version != 1 {
		return s, fmt.Errorf("unsupported version %d", stored.Version)
	}
	for key, record := range stored.Sessions {
		if key != "" && record.ResumeID != "" && filepath.IsAbs(record.CWD) && record.HistoryCount > 0 && record.HistoryHash != "" {
			s.records[key] = record
		}
	}
	return s, nil
}

func (s *sessionStore) recordsSnapshot() map[string]persistedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]persistedSession, len(s.records))
	for key, record := range s.records {
		result[key] = record
	}
	return result
}

func (s *sessionStore) put(key string, record persistedSession) error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[key] = record
	return s.writeLocked()
}

func (s *sessionStore) delete(key string) error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[key]; !ok {
		return nil
	}
	delete(s.records, key)
	return s.writeLocked()
}

func (s *sessionStore) writeLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	data, err := json.MarshalIndent(persistedSessions{Version: 1, Sessions: s.records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}
