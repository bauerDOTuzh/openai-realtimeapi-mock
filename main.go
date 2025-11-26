package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// --- Shared Types ---

type BaseEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
}

type SessionObject struct {
	ID               string        `json:"id"`
	Object           string        `json:"object"` // "realtime.session"
	ClientSecret     *ClientSecret `json:"client_secret,omitempty"`
	Model            string        `json:"model,omitempty"`
	InputAudioFormat string        `json:"input_audio_format,omitempty"`
	Modalities       []string      `json:"modalities,omitempty"`
}

type ClientSecret struct {
	Value     string `json:"value"`
	ExpiresAt int64  `json:"expires_at"`
}

type ConversationObject struct {
	ID     string `json:"id"`
	Object string `json:"object"` // "realtime.conversation"
}

type RecordedEvent struct {
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// --- Global Variables ---

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Allow all origins
}

// --- Safe WebSocket ---

type SafeWebSocket struct {
	Conn *websocket.Conn
	Mu   sync.Mutex
}

func (s *SafeWebSocket) WriteMessage(messageType int, data []byte) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.Conn.WriteMessage(messageType, data)
}

func (s *SafeWebSocket) ReadMessage() (messageType int, p []byte, err error) {
	// ReadMessage is not concurrent-safe either, but usually we have one reader.
	// If we needed concurrent reads, we'd lock here too.
	// For now, we assume single reader loop.
	return s.Conn.ReadMessage()
}

func (s *SafeWebSocket) Close() error {
	return s.Conn.Close()
}

func (s *SafeWebSocket) RemoteAddr() string {
	return s.Conn.RemoteAddr().String()
}

// --- Main Function ---

func main() {
	initConfig()

	// Setup HTTP Routes
	router := setupRouter()

	// Start Server
	addr := fmt.Sprintf(":%d", appConfig.Server.Port)
	log.Printf("Starting Simplified OpenAI Realtime Mock server on %s", addr)
	log.Printf("Active Mode: %s", appConfig.Mode)
	if appConfig.Mode == "proxy" {
		log.Printf("Proxy Target: %s", appConfig.Proxy.URL)
		log.Printf("Proxy Model: %s", appConfig.Proxy.Model)
	} else {
		log.Printf("Loaded %d scenarios", len(appConfig.Scenarios))
		for _, s := range appConfig.Scenarios {
			log.Printf("- Scenario: %s (%d events)", s.Name, len(s.Events))
		}
	}

	err := http.ListenAndServe(addr, router)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// setupRouter initializes the HTTP routes.
func setupRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/realtime/sessions", handleCreateSession)
	mux.HandleFunc("/v1/realtime", handleWebSocket)
	return mux
}

// --- HTTP Handlers ---

func handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	// Ignore request body, just send back a success with a fake token
	sessionID := "mock-sess-" + uuid.NewString()
	ephemeralKey := "ek_mock_" + uuid.NewString()
	// ExpiresAt should be in milliseconds for consistency with typical client expectations
	expiresAt := time.Now().Add(1 * time.Minute).UnixMilli()

	response := SessionObject{
		ID:               sessionID,
		Object:           "realtime.session",
		Model:            "mock-model", // Add minimal fields client might need
		InputAudioFormat: "pcm16",
		Modalities:       []string{"audio", "text"},
		ClientSecret: &ClientSecret{
			Value:     ephemeralKey,
			ExpiresAt: expiresAt,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	log.Printf("Issued mock session token for session: %s", sessionID)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check Mode
	if appConfig.Mode == "proxy" {
		handleProxyWebSocket(w, r)
		return
	}

	// Mock Mode
	handleMockWebSocket(w, r)
}

// --- Shared Helpers ---

func sendJSONEvent(conn *SafeWebSocket, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal event: %v", err)
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}
