package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- Proxy Mode Logic ---

func handleProxyWebSocket(w http.ResponseWriter, r *http.Request) {
	// 1. Upgrade Client Connection
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Proxy: WebSocket upgrade error: %v", err)
		return
	}
	safeClientConn := &SafeWebSocket{Conn: clientConn}
	defer safeClientConn.Close()
	log.Printf("Proxy: Client connected: %s", safeClientConn.RemoteAddr())

	// 2. Connect to OpenAI Realtime API
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Printf("Proxy: Error - OPENAI_API_KEY environment variable not set")
		safeClientConn.WriteMessage(websocket.TextMessage, []byte(`{"type": "error", "error": {"message": "OPENAI_API_KEY not set on server"}}`))
		return
	}

	model := appConfig.Proxy.Model
	if model == "" {
		model = "gpt-4o-mini-realtime-preview-2024-12-17" // Fallback default
	}
	targetURL := fmt.Sprintf("%s?model=%s", appConfig.Proxy.URL, model)
	log.Printf("Proxy: Connecting to OpenAI at %s", targetURL)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+apiKey)
	header.Set("OpenAI-Beta", "realtime=v1")

	openaiConn, _, err := websocket.DefaultDialer.Dial(targetURL, header)
	if err != nil {
		log.Printf("Proxy: Failed to connect to OpenAI: %v", err)
		safeClientConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type": "error", "error": {"message": "Failed to connect to OpenAI: %v"}}`, err)))
		return
	}
	defer openaiConn.Close()
	log.Printf("Proxy: Connected to OpenAI")

	// 3. Setup Recording
	recordingDir := appConfig.Proxy.RecordingPath
	if recordingDir == "" {
		recordingDir = "recordings"
	}
	if err := os.MkdirAll(recordingDir, 0755); err != nil {
		log.Printf("Proxy: Failed to create recording directory: %v", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	recordingFile := filepath.Join(recordingDir, fmt.Sprintf("%s.ndjson", timestamp))
	recFile, err := os.OpenFile(recordingFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Proxy: Failed to open recording file: %v", err)
	} else {
		log.Printf("Proxy: Recording to %s", recordingFile)
		defer recFile.Close()
	}

	// Helper to record messages
	recordMessage := func(direction string, msg []byte) {
		if recFile == nil {
			return
		}
		// We only record JSON text messages for replayability
		if json.Valid(msg) {
			event := RecordedEvent{
				Timestamp: time.Now().UnixMilli(),
				Data:      json.RawMessage(msg),
			}
			line, err := json.Marshal(event)
			if err != nil {
				log.Printf("Proxy: Error marshaling recorded event: %v", err)
				return
			}
			line = append(line, '\n')
			if _, err := recFile.Write(line); err != nil {
				log.Printf("Proxy: Error writing to recording file: %v", err)
			}
		}
	}

	// 4. Bi-directional Forwarding
	var wg sync.WaitGroup
	wg.Add(2)

	// Inbound Recorder (Client -> Server)
	var inboundRecorder *Recorder
	if appConfig.LogInboundMessages {
		var err error
		inboundRecorder, err = NewRecorder(appConfig.Proxy.RecordingPath, "inbound")
		if err != nil {
			log.Printf("Proxy: Failed to initialize inbound recorder: %v", err)
		} else {
			defer inboundRecorder.Close()
		}
	}

	// Client -> OpenAI
	go func() {
		defer wg.Done()
		for {
			msgType, msg, err := safeClientConn.ReadMessage()
			if err != nil {
				log.Printf("Proxy: Client read error: %v", err)
				openaiConn.Close() // Close upstream to stop the other loop
				break
			}

			// Record inbound message
			if inboundRecorder != nil && msgType == websocket.TextMessage {
				inboundRecorder.RecordMessage(msg)
			}

			// Forward to OpenAI
			if err := openaiConn.WriteMessage(msgType, msg); err != nil {
				log.Printf("Proxy: Error writing to OpenAI: %v", err)
				break
			}
		}
	}()

	// OpenAI -> Client
	go func() {
		defer wg.Done()
		for {
			msgType, msg, err := openaiConn.ReadMessage()
			if err != nil {
				log.Printf("Proxy: OpenAI read error: %v", err)
				safeClientConn.Close() // Close downstream
				break
			}

			// Record incoming message (from OpenAI) - Existing full recording
			if msgType == websocket.TextMessage {
				recordMessage("inbound", msg)
			}

			// Forward to Client
			if err := safeClientConn.WriteMessage(msgType, msg); err != nil {
				log.Printf("Proxy: Error writing to Client: %v", err)
				break
			}
		}
	}()

	wg.Wait()
	log.Printf("Proxy: Session ended")
}
