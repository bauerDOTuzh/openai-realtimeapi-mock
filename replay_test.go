package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHandleWebSocket_Replay(t *testing.T) {
	// 1. Setup Config with recording path
	loadTestConfig(t)
	// Override recording path to a temp dir
	tempDir := t.TempDir()
	appConfig.Proxy.RecordingPath = tempDir

	// 2. Create a dummy recording file
	recordingName := "test_replay.ndjson"
	recordingPath := filepath.Join(tempDir, recordingName)

	// Create some dummy events
	events := []map[string]interface{}{
		{"type": "session.created", "event_id": "evt_1"},
		{"type": "conversation.item.created", "event_id": "evt_2"},
		{"type": "response.audio_transcript.delta", "delta": "Hello from replay", "event_id": "evt_3"},
	}

	f, err := os.Create(recordingPath)
	if err != nil {
		t.Fatalf("Failed to create recording file: %v", err)
	}

	baseTime := time.Now().UnixMilli()
	for i, evt := range events {
		data, _ := json.Marshal(evt)

		// Create RecordedEvent
		recEvt := RecordedEvent{
			Timestamp: baseTime + int64(i*50), // 50ms delay between events
			Data:      json.RawMessage(data),
		}

		line, _ := json.Marshal(recEvt)
		f.Write(line)
		f.Write([]byte("\n"))
	}
	f.Close()

	// 3. Start Server
	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	// 4. Connect with replay scenario
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?replaySession=" + recordingName

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	// 5. Read messages
	// We expect:
	// 1. session.created (from handleMockWebSocket logic)
	// 2. conversation.created (from handleMockWebSocket logic)
	// 3. Replayed messages (session.created, conversation.item.created, response.text.delta)

	// Note: The current implementation sends a fresh session.created/conversation.created BEFORE replaying.
	// So we should see those first.

	// Read standard welcome messages
	for i := 0; i < 2; i++ {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Error reading welcome message %d: %v", i, err)
		}
	}

	// Send trigger (input_audio_buffer.append) to start replay
	conn.WriteJSON(map[string]string{
		"type":  "input_audio_buffer.append",
		"audio": "AAA=",
	})

	// Now we expect the replayed messages
	// We look for the unique "Hello from replay" delta
	foundReplayText := false

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				break // Stop reading after timeout
			}
			t.Fatalf("Read error: %v", err)
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg["type"] == "response.audio_transcript.delta" {
			if delta, ok := msg["delta"].(string); ok {
				if delta == "Hello from replay" {
					foundReplayText = true
					break
				}
			}
		}
	}

	if !foundReplayText {
		t.Error("Did not receive replayed text message")
	}
}
