package main

import (
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

	// 3. Setup Recording based on config
	recordingName := r.URL.Query().Get("recording_name")
	recordingDir := appConfig.Proxy.RecordingPath
	if recordingDir == "" {
		recordingDir = "recordings"
	}

	// Generate base name for this session
	var baseName string
	if recordingName != "" {
		baseName = filepath.Base(recordingName)
	} else {
		baseName = time.Now().Format("2006-01-02_15-04-05")
	}

	// Inbound Recorder (Client -> Server) - controlled by logInbound config
	var inboundRecorder *Recorder
	if appConfig.LogInbound {
		inboundName := "inbound_" + baseName
		inboundRecorder, err = NewRecorder(recordingDir, "inbound", inboundName)
		if err != nil {
			log.Printf("Proxy: Failed to initialize inbound recorder: %v", err)
		} else {
			defer inboundRecorder.Close()
		}
	}

	// Outbound Recorder (Server -> Client) - controlled by logOutbound config (proxy mode only)
	var outboundRecorder *Recorder
	if appConfig.LogOutbound {
		outboundName := "outbound_" + baseName
		outboundRecorder, err = NewRecorder(recordingDir, "outbound", outboundName)
		if err != nil {
			log.Printf("Proxy: Failed to initialize outbound recorder: %v", err)
		} else {
			defer outboundRecorder.Close()
		}
	}

	// 4. Bi-directional Forwarding
	var wg sync.WaitGroup
	wg.Add(2)

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

			// Record inbound message (client -> OpenAI)
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

			// Record outbound message (OpenAI -> client)
			if outboundRecorder != nil && msgType == websocket.TextMessage {
				outboundRecorder.RecordMessage(msg)
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
