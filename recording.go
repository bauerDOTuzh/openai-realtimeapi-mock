package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Recorder handles logging of messages to an NDJSON file.
type Recorder struct {
	file *os.File
	mu   sync.Mutex
}

// NewRecorder creates a new Recorder instance.
// It creates the file with a timestamped name in the specified directory.
// prefix is used for the filename (e.g., "inbound", "proxy").
func NewRecorder(dir string, prefix string) (*Recorder, error) {
	if dir == "" {
		dir = "recordings"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create recording directory: %w", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	filename := fmt.Sprintf("%s_%s.ndjson", prefix, timestamp)
	path := filepath.Join(dir, filename)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open recording file: %w", err)
	}

	log.Printf("Recording %s messages to %s", prefix, path)
	return &Recorder{file: f}, nil
}

// RecordMessage logs a JSON message to the file.
func (r *Recorder) RecordMessage(msg []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return
	}

	// Ensure it's valid JSON
	if !json.Valid(msg) {
		return
	}

	event := RecordedEvent{
		Timestamp: time.Now().UnixMilli(),
		Data:      json.RawMessage(msg),
	}

	line, err := json.Marshal(event)
	if err != nil {
		log.Printf("Error marshaling recorded event: %v", err)
		return
	}

	line = append(line, '\n')
	if _, err := r.file.Write(line); err != nil {
		log.Printf("Error writing to recording file: %v", err)
	}
}

// Close closes the underlying file.
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
}
