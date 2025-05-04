package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// --- Configuration Structs ---

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type MockConfig struct {
	ResponseDelaySeconds int    `yaml:"responseDelaySeconds"`
	AudioWavPath         string `yaml:"audioWavPath"`
	TranscriptText       string `yaml:"transcriptText"`
	ChunkIntervalMs      int    `yaml:"chunkIntervalMs"`
	AudioChunkSizeBytes  int    `yaml:"audioChunkSizeBytes"`
}

type Config struct {
	Server ServerConfig `yaml:"server"`
	Mock   MockConfig   `yaml:"mock"`
}

// --- Basic Event Structs (Minimal) ---

type BaseEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
}

// Used for session creation response and session.created event
type SessionObject struct {
	ID           string        `json:"id"`
	Object       string        `json:"object"` // "realtime.session"
	ClientSecret *ClientSecret `json:"client_secret,omitempty"`
	// Add other fields only if strictly needed by your client for init
	Model            string   `json:"model,omitempty"`
	InputAudioFormat string   `json:"input_audio_format,omitempty"`
	Modalities       []string `json:"modalities,omitempty"`
}

type ClientSecret struct {
	Value     string `json:"value"`
	ExpiresAt int64  `json:"expires_at"`
}

type ConversationObject struct {
	ID     string `json:"id"`
	Object string `json:"object"` // "realtime.conversation"
}

// --- Global Variables ---

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Allow all origins
}

var appConfig Config // Loaded config

// --- Main Function ---

func main() {
	configFile := flag.String("config", "config.yaml", "Path to the configuration file")
	flag.Parse()

	// Load Config
	data, err := os.ReadFile(*configFile)
	if err != nil {
		log.Fatalf("Failed to read config file %s: %v", *configFile, err)
	}
	err = yaml.Unmarshal(data, &appConfig)
	if err != nil {
		log.Fatalf("Failed to parse config file %s: %v", *configFile, err)
	}

	// Basic validation/defaults
	if appConfig.Server.Host == "" {
		appConfig.Server.Host = "localhost"
	}
	if appConfig.Server.Port == 0 {
		appConfig.Server.Port = 8080
	}
	if appConfig.Mock.AudioChunkSizeBytes == 0 {
		appConfig.Mock.AudioChunkSizeBytes = 4096
	}
	if appConfig.Mock.ChunkIntervalMs == 0 {
		appConfig.Mock.ChunkIntervalMs = 100
	}

	// Check if audio file exists
	if _, err := os.Stat(appConfig.Mock.AudioWavPath); os.IsNotExist(err) {
		log.Printf("WARNING: Audio file specified in config does not exist: %s", appConfig.Mock.AudioWavPath)
		log.Printf("WARNING: Audio playback will fail.")
	} else {
		log.Printf("Audio file found: %s", appConfig.Mock.AudioWavPath)
	}

	// Setup HTTP Routes
	// Minimal session endpoint to allow WebSocket connection (often needs a token)
	http.HandleFunc("/v1/realtime/sessions", handleCreateSession)
	// WebSocket endpoint
	http.HandleFunc("/v1/realtime", handleWebSocket)

	// Start Server
	addr := fmt.Sprintf("%s:%d", appConfig.Server.Host, appConfig.Server.Port)
	log.Printf("Starting Simplified OpenAI Realtime Mock server on %s", addr)
	log.Printf("Response Delay: %d seconds", appConfig.Mock.ResponseDelaySeconds)
	log.Printf("Audio File: %s", appConfig.Mock.AudioWavPath)
	log.Printf("Transcript: %s", appConfig.Mock.TranscriptText)
	err = http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
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
	expiresAt := time.Now().Add(1 * time.Minute).Unix()

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
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()
	log.Printf("WebSocket client connected: %s", conn.RemoteAddr())

	// --- Send Welcome Messages (SessionCreated, ConversationCreated) ---
	sessionID := "mock-ws-sess-" + uuid.NewString()
	convID := "mock-conv-" + uuid.NewString()

	// Send session.created
	sessionCreated := map[string]interface{}{
		"type":     "session.created",
		"event_id": uuid.NewString(),
		"session": SessionObject{
			ID:               sessionID,
			Object:           "realtime.session",
			Model:            "mock-model",
			InputAudioFormat: "pcm16",
			Modalities:       []string{"audio", "text"},
		},
	}
	if err := sendJSONEvent(conn, sessionCreated); err != nil {
		return
	}

	// Send conversation.created
	convCreated := map[string]interface{}{
		"type":     "conversation.created",
		"event_id": uuid.NewString(),
		"conversation": ConversationObject{
			ID:     convID,
			Object: "realtime.conversation",
		},
	}
	if err := sendJSONEvent(conn, convCreated); err != nil {
		return
	}

	// --- Simple Client State ---
	var responseTimer *time.Timer // Timer to trigger response
	var timerMutex sync.Mutex     // Protect the timer
	audioReceived := false        // Flag to start timer only once

	// --- Read Loop ---
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %s read error: %v", conn.RemoteAddr(), err)
			} else {
				log.Printf("Client %s disconnected: %v", conn.RemoteAddr(), err)
			}
			break // Exit loop on error or close
		}

		if messageType == websocket.TextMessage {
			var base BaseEvent
			if err := json.Unmarshal(message, &base); err == nil {
				log.Printf("Client %s received event: %s", conn.RemoteAddr(), base.Type)

				// --- The Core Logic: Start Timer on First Audio ---
				if base.Type == "input_audio_buffer.append" {
					timerMutex.Lock()
					if !audioReceived {
						audioReceived = true
						log.Printf("Client %s: First audio received. Starting %ds response timer.", conn.RemoteAddr(), appConfig.Mock.ResponseDelaySeconds)

						// Stop existing timer if any (shouldn't happen with audioReceived flag, but safe)
						if responseTimer != nil {
							responseTimer.Stop()
						}

						// Start the timer
						responseTimer = time.AfterFunc(time.Duration(appConfig.Mock.ResponseDelaySeconds)*time.Second, func() {
							log.Printf("Client %s: Response timer fired. Starting audio/transcript stream.", conn.RemoteAddr())
							// Run response streaming in a new goroutine to avoid blocking timer callback
							go streamResponse(conn, sessionID, convID)
						})
					}
					timerMutex.Unlock()
				}
				// Add simple handlers for other messages if needed (e.g., session.update ack)
				// else if base.Type == "session.update" {
				//     // Acknowledge minimally
				//     ack := map[string]interface{}{ "type": "session.updated", ... }
				//     sendJSONEvent(conn, ack)
				// }

			} else {
				log.Printf("Client %s received non-JSON text message or parse error: %v", conn.RemoteAddr(), err)
			}
		} else if messageType == websocket.BinaryMessage {
			log.Printf("Client %s received binary message (%d bytes) - treating as audio", conn.RemoteAddr(), len(message))
			// --- Also Trigger Timer Here if Binary is Used for Audio ---
			timerMutex.Lock()
			if !audioReceived {
				audioReceived = true
				log.Printf("Client %s: First binary audio received. Starting %ds response timer.", conn.RemoteAddr(), appConfig.Mock.ResponseDelaySeconds)
				if responseTimer != nil {
					responseTimer.Stop()
				} // Safety stop
				responseTimer = time.AfterFunc(time.Duration(appConfig.Mock.ResponseDelaySeconds)*time.Second, func() {
					log.Printf("Client %s: Response timer fired (from binary). Starting audio/transcript stream.", conn.RemoteAddr())
					go streamResponse(conn, sessionID, convID)
				})
			}
			timerMutex.Unlock()
		}
	}

	// Cleanup timer if the read loop exits before it fires
	timerMutex.Lock()
	if responseTimer != nil {
		responseTimer.Stop()
		log.Printf("Client %s: Cleaned up response timer on disconnect.", conn.RemoteAddr())
	}
	timerMutex.Unlock()
}

// --- Response Streaming Logic ---

func streamResponse(conn *websocket.Conn, sessionID, convID string) {
	responseID := "mock-resp-" + uuid.NewString()
	itemID := "mock-item-" + uuid.NewString() // ID for the simulated response message
	contentIndexAudio := 0                    // Assuming audio is first content part
	contentIndexText := 1                     // Assuming transcript is second (or handle dynamically)

	log.Printf("Client %s: Streaming response (RespID: %s)", conn.RemoteAddr(), responseID)

	// --- Send Response Lifecycle Start (Optional but good practice) ---
	// Send response.created
	respCreated := map[string]interface{}{"type": "response.created", "event_id": uuid.NewString(), "response": map[string]interface{}{"id": responseID, "object": "realtime.response", "status": "in_progress"}}
	if err := sendJSONEvent(conn, respCreated); err != nil {
		return
	}

	// Send response.output_item.added (for the message containing audio/text)
	itemAdded := map[string]interface{}{"type": "response.output_item.added", "event_id": uuid.NewString(), "response_id": responseID, "output_index": 0, "item": map[string]interface{}{"id": itemID, "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": []interface{}{}}}
	if err := sendJSONEvent(conn, itemAdded); err != nil {
		return
	}

	// Send response.content_part.added (for audio)
	audioPartAdded := map[string]interface{}{"type": "response.content_part.added", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexAudio, "part": map[string]interface{}{"type": "audio"}}
	if err := sendJSONEvent(conn, audioPartAdded); err != nil {
		return
	}

	// Send response.content_part.added (for transcript - using 'text' type)
	textPartAdded := map[string]interface{}{"type": "response.content_part.added", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexText, "part": map[string]interface{}{"type": "text"}} // Represent transcript as text
	if err := sendJSONEvent(conn, textPartAdded); err != nil {
		return
	}

	// --- Start Streaming Goroutines ---
	var wg sync.WaitGroup
	audioDone := make(chan bool, 1)
	transcriptDone := make(chan bool, 1)

	// Goroutine for Audio Streaming
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamAudio(conn, responseID, itemID, contentIndexAudio)
		audioDone <- true
	}()

	// Goroutine for Transcript Streaming
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamTranscript(conn, responseID, itemID, contentIndexText)
		transcriptDone <- true
	}()

	// Wait for both streams to finish
	wg.Wait()
	<-audioDone
	<-transcriptDone

	// --- Send Response Lifecycle End (Optional but good practice) ---
	// Send response.content_part.done (for audio & text) - simplified, send after all streaming
	audioPartDone := map[string]interface{}{"type": "response.content_part.done", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexAudio, "part": map[string]interface{}{"type": "audio"}}
	if err := sendJSONEvent(conn, audioPartDone); err != nil {
		return
	}
	textPartDone := map[string]interface{}{"type": "response.content_part.done", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexText, "part": map[string]interface{}{"type": "text", "text": appConfig.Mock.TranscriptText}}
	if err := sendJSONEvent(conn, textPartDone); err != nil {
		return
	}

	// Send response.output_item.done
	itemDone := map[string]interface{}{"type": "response.output_item.done", "event_id": uuid.NewString(), "response_id": responseID, "output_index": 0, "item": map[string]interface{}{"id": itemID, "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "audio"}, map[string]interface{}{"type": "text", "text": appConfig.Mock.TranscriptText}}}}
	if err := sendJSONEvent(conn, itemDone); err != nil {
		return
	}

	// Send response.done
	respDone := map[string]interface{}{"type": "response.done", "event_id": uuid.NewString(), "response": map[string]interface{}{"id": responseID, "object": "realtime.response", "status": "completed", "output": []interface{}{map[string]interface{}{"id": itemID, "object": "realtime.item", "type": "message", "status": "completed"}}}} // Simplified output
	if err := sendJSONEvent(conn, respDone); err != nil {
		return
	}

	log.Printf("Client %s: Finished streaming response (RespID: %s)", conn.RemoteAddr(), responseID)
}

func streamAudio(conn *websocket.Conn, responseID, itemID string, contentIndex int) {
	file, err := os.Open(appConfig.Mock.AudioWavPath)
	if err != nil {
		log.Printf("Client %s: ERROR opening audio file %s: %v", conn.RemoteAddr(), appConfig.Mock.AudioWavPath, err)
		sendErrorEvent(conn, "audio_file_error", fmt.Sprintf("Failed to open audio file: %v", err))
		return
	}
	defer file.Close()

	// Skip WAV header (assume 44 bytes)
	header := make([]byte, 44)
	if _, err = io.ReadFull(file, header); err != nil {
		log.Printf("Client %s: ERROR reading WAV header from %s: %v", conn.RemoteAddr(), appConfig.Mock.AudioWavPath, err)
		sendErrorEvent(conn, "audio_file_error", fmt.Sprintf("Failed to read audio header: %v", err))
		return
	}

	buffer := make([]byte, appConfig.Mock.AudioChunkSizeBytes)
	ticker := time.NewTicker(time.Duration(appConfig.Mock.ChunkIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		n, err := file.Read(buffer)
		if n > 0 {
			audioData := buffer[:n]
			encodedData := base64.StdEncoding.EncodeToString(audioData)

			audioDelta := map[string]interface{}{
				"type":          "response.audio.delta",
				"event_id":      uuid.NewString(),
				"response_id":   responseID,
				"item_id":       itemID,
				"output_index":  0, // Assuming single output item
				"content_index": contentIndex,
				"delta":         encodedData,
			}
			if err := sendJSONEvent(conn, audioDelta); err != nil {
				log.Printf("Client %s: Error sending audio delta: %v", conn.RemoteAddr(), err)
				return // Stop streaming on write error
			}
		}

		if err == io.EOF {
			log.Printf("Client %s: Reached EOF for audio file %s", conn.RemoteAddr(), appConfig.Mock.AudioWavPath)
			break // End of file
		}
		if err != nil {
			log.Printf("Client %s: ERROR reading WAV data from %s: %v", conn.RemoteAddr(), appConfig.Mock.AudioWavPath, err)
			sendErrorEvent(conn, "audio_file_error", fmt.Sprintf("Error reading audio data: %v", err))
			break // Stop on read error
		}
	}

	// Send response.audio.done (optional but good practice)
	audioDoneEvent := map[string]interface{}{"type": "response.audio.done", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndex}
	sendJSONEvent(conn, audioDoneEvent) // Ignore error on final done event
	log.Printf("Client %s: Finished streaming audio chunks.", conn.RemoteAddr())

}

func streamTranscript(conn *websocket.Conn, responseID, itemID string, contentIndex int) {
	words := strings.Fields(appConfig.Mock.TranscriptText)
	ticker := time.NewTicker(time.Duration(appConfig.Mock.ChunkIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	wordIndex := 0
	fullSentTranscript := ""

	for range ticker.C {
		if wordIndex >= len(words) {
			break // Done sending transcript
		}

		delta := words[wordIndex] + " "
		fullSentTranscript += delta

		// Send transcript delta (using 'response.text.delta' as we added a 'text' content part)
		transcriptDelta := map[string]interface{}{
			"type":          "response.text.delta",
			"event_id":      uuid.NewString(),
			"response_id":   responseID,
			"item_id":       itemID,
			"output_index":  0, // Assuming single output item
			"content_index": contentIndex,
			"delta":         delta,
		}
		if err := sendJSONEvent(conn, transcriptDelta); err != nil {
			log.Printf("Client %s: Error sending transcript delta: %v", conn.RemoteAddr(), err)
			return // Stop streaming on write error
		}
		wordIndex++
	}

	// Send response.text.done (optional but good practice)
	textDoneEvent := map[string]interface{}{"type": "response.text.done", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndex, "text": strings.TrimSpace(fullSentTranscript)}
	sendJSONEvent(conn, textDoneEvent) // Ignore error on final done event

	log.Printf("Client %s: Finished streaming transcript chunks.", conn.RemoteAddr())
}

// --- Helper Functions ---

var wsWriteMutex sync.Mutex // Mutex to protect concurrent writes to the same WebSocket connection

// sendJSONEvent marshals and sends a JSON event over WebSocket safely
func sendJSONEvent(conn *websocket.Conn, event interface{}) error {
	wsWriteMutex.Lock()
	defer wsWriteMutex.Unlock()
	bytes, err := json.Marshal(event)
	if err != nil {
		log.Printf("Client %s: Error marshalling event type %T: %v", conn.RemoteAddr(), event, err)
		// Try sending a raw error string if marshalling the error event itself might fail
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"internal_error","message":"Failed to marshal server event"}}`))
		return fmt.Errorf("marshalling error: %w", err)
	}
	err = conn.WriteMessage(websocket.TextMessage, bytes)
	if err != nil {
		log.Printf("Client %s: Error writing message type %T: %v", conn.RemoteAddr(), event, err)
	}
	return err
}

// sendErrorEvent sends a structured error event
func sendErrorEvent(conn *websocket.Conn, code, message string) {
	errEvent := map[string]interface{}{
		"type":     "error",
		"event_id": uuid.NewString(),
		"error": map[string]interface{}{
			"type":    "mock_server_error", // Custom type for mock errors
			"code":    code,
			"message": message,
		},
	}
	// Use the mutex-protected send function
	_ = sendJSONEvent(conn, errEvent)
}
