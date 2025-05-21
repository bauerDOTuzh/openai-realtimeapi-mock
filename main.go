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
	"path/filepath"
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

const (
	customConfigPath = "/app/custom_config/config.yaml"
	// Default config path if -config flag is not provided or for Docker's CMD
	// This will be effectively /app/config/config.yaml in Docker as per Dockerfile CMD.
	// For local dev, it's "config.yaml" in the current dir.
	defaultConfigFlagValue = "config.yaml"
)

// loadConfiguration loads the application configuration.
// It prioritizes the customConfigPath, then the path from -config flag.
// It also resolves the audioWavPath to be relative to the loaded config file's directory.
func loadConfiguration(cliConfigPath string) (string, error) {
	var selectedConfigFile string

	log.Printf("Attempting to load custom config from %s", customConfigPath)
	if _, err := os.Stat(customConfigPath); err == nil {
		log.Printf("Custom config file found at %s. Loading it.", customConfigPath)
		selectedConfigFile = customConfigPath
	} else if os.IsNotExist(err) {
		log.Printf("Custom config file not found at %s. Using config path from -config flag: %s", customConfigPath, cliConfigPath)
		selectedConfigFile = cliConfigPath
	} else {
		// Other error checking customConfigPath (e.g., permission denied)
		return "", fmt.Errorf("error checking custom config file %s: %w", customConfigPath, err)
	}

	log.Printf("Loading configuration from: %s", selectedConfigFile)
	data, err := os.ReadFile(selectedConfigFile)
	if err != nil {
		return selectedConfigFile, fmt.Errorf("failed to read config file %s: %w", selectedConfigFile, err)
	}
	err = yaml.Unmarshal(data, &appConfig)
	if err != nil {
		return selectedConfigFile, fmt.Errorf("failed to parse config file %s: %w", selectedConfigFile, err)
	}

	// Resolve audioWavPath to be relative to the loaded config file's directory
	if appConfig.Mock.AudioWavPath != "" && !filepath.IsAbs(appConfig.Mock.AudioWavPath) {
		configDir := filepath.Dir(selectedConfigFile)
		resolvedAudioPath := filepath.Join(configDir, appConfig.Mock.AudioWavPath)
		log.Printf("Original audioWavPath: '%s'. Config file directory: '%s'. Resolved audioWavPath to: '%s'", appConfig.Mock.AudioWavPath, configDir, resolvedAudioPath)
		appConfig.Mock.AudioWavPath = resolvedAudioPath
	} else {
		log.Printf("audioWavPath '%s' is absolute or empty, using as is.", appConfig.Mock.AudioWavPath)
	}
	return selectedConfigFile, nil
}

// --- Main Function ---
func main() {
	cliConfigPath := flag.String("config", defaultConfigFlagValue, "Path to the configuration file")
	flag.Parse()

	loadedConfigFile, err := loadConfiguration(*cliConfigPath)
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	log.Printf("Successfully loaded and processed configuration from %s", loadedConfigFile)

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

	// Check if audio file exists (after path resolution)
	if appConfig.Mock.AudioWavPath != "" { // Only check if a path is configured
		if _, err := os.Stat(appConfig.Mock.AudioWavPath); os.IsNotExist(err) {
			log.Printf("WARNING: Audio file specified in config does not exist: %s", appConfig.Mock.AudioWavPath)
			log.Printf("WARNING: Audio playback will fail if this path is used.")
		} else {
			log.Printf("Audio file found: %s", appConfig.Mock.AudioWavPath)
		}
	} else {
		log.Printf("WARNING: No audioWavPath configured. Audio playback will not occur.")
	}


	// Setup HTTP Routes
	router := setupRouter()

	// Start Server
	addr := fmt.Sprintf("%s:%d", appConfig.Server.Host, appConfig.Server.Port)
	log.Printf("Starting Simplified OpenAI Realtime Mock server on %s", addr)
	log.Printf("Response Delay: %d seconds", appConfig.Mock.ResponseDelaySeconds)
	log.Printf("Using Audio File: %s", appConfig.Mock.AudioWavPath) // Log the potentially resolved path
	log.Printf("Transcript: %s", appConfig.Mock.TranscriptText)
	err = http.ListenAndServe(addr, router) // Use the router
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// setupRouter initializes the HTTP routes.
// This function is used by main and can be used by tests.
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
			timerMutex.Lock()
			if !audioReceived {
				audioReceived = true
				log.Printf("Client %s: First binary audio received. Starting %ds response timer.", conn.RemoteAddr(), appConfig.Mock.ResponseDelaySeconds)
				if responseTimer != nil {
					responseTimer.Stop()
				} 
				responseTimer = time.AfterFunc(time.Duration(appConfig.Mock.ResponseDelaySeconds)*time.Second, func() {
					log.Printf("Client %s: Response timer fired (from binary). Starting audio/transcript stream.", conn.RemoteAddr())
					go streamResponse(conn, sessionID, convID)
				})
			}
			timerMutex.Unlock()
		}
	}

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
	itemID := "mock-item-" + uuid.NewString() 
	contentIndexAudio := 0                   
	contentIndexText := 1                    

	log.Printf("Client %s: Streaming response (RespID: %s)", conn.RemoteAddr(), responseID)

	respCreated := map[string]interface{}{"type": "response.created", "event_id": uuid.NewString(), "response": map[string]interface{}{"id": responseID, "object": "realtime.response", "status": "in_progress"}}
	if err := sendJSONEvent(conn, respCreated); err != nil {
		return
	}

	itemAdded := map[string]interface{}{"type": "response.output_item.added", "event_id": uuid.NewString(), "response_id": responseID, "output_index": 0, "item": map[string]interface{}{"id": itemID, "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": []interface{}{}}}
	if err := sendJSONEvent(conn, itemAdded); err != nil {
		return
	}

	if appConfig.Mock.AudioWavPath != "" {
		audioPartAdded := map[string]interface{}{"type": "response.content_part.added", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexAudio, "part": map[string]interface{}{"type": "audio"}}
		if err := sendJSONEvent(conn, audioPartAdded); err != nil {
			return
		}
	}

	if appConfig.Mock.TranscriptText != "" {
		textPartAdded := map[string]interface{}{"type": "response.content_part.added", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexText, "part": map[string]interface{}{"type": "text"}} 
		if err := sendJSONEvent(conn, textPartAdded); err != nil {
			return
		}
	}

	var wg sync.WaitGroup
	audioDone := make(chan bool, 1)
	transcriptDone := make(chan bool, 1)

	if appConfig.Mock.AudioWavPath != "" { // Only stream audio if path is configured
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamAudio(conn, responseID, itemID, contentIndexAudio)
			audioDone <- true
		}()
	} else {
		close(audioDone) // If no audio, signal done immediately
	}

	if appConfig.Mock.TranscriptText != "" { // Only stream transcript if text is configured
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamTranscript(conn, responseID, itemID, contentIndexText)
			transcriptDone <- true
		}()
	} else {
		close(transcriptDone) // If no transcript, signal done immediately
	}

	wg.Wait()
	<-audioDone
	<-transcriptDone

	if appConfig.Mock.AudioWavPath != "" {
		audioPartDone := map[string]interface{}{"type": "response.content_part.done", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexAudio, "part": map[string]interface{}{"type": "audio"}}
		if err := sendJSONEvent(conn, audioPartDone); err != nil { return }
	}
	if appConfig.Mock.TranscriptText != "" {
		textPartDone := map[string]interface{}{"type": "response.content_part.done", "event_id": uuid.NewString(), "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": contentIndexText, "part": map[string]interface{}{"type": "text", "text": appConfig.Mock.TranscriptText}}
		if err := sendJSONEvent(conn, textPartDone); err != nil { return }
	}
	
	// Construct content for itemDone based on what was actually streamed
	itemDoneContent := []interface{}{}
	if appConfig.Mock.AudioWavPath != "" {
		itemDoneContent = append(itemDoneContent, map[string]interface{}{"type": "audio"})
	}
	if appConfig.Mock.TranscriptText != "" {
		itemDoneContent = append(itemDoneContent, map[string]interface{}{"type": "text", "text": appConfig.Mock.TranscriptText})
	}


	itemDone := map[string]interface{}{"type": "response.output_item.done", "event_id": uuid.NewString(), "response_id": responseID, "output_index": 0, "item": map[string]interface{}{"id": itemID, "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": itemDoneContent}}
	if err := sendJSONEvent(conn, itemDone); err != nil {
		return
	}

	respDone := map[string]interface{}{"type": "response.done", "event_id": uuid.NewString(), "response": map[string]interface{}{"id": responseID, "object": "realtime.response", "status": "completed", "output": []interface{}{map[string]interface{}{"id": itemID, "object": "realtime.item", "type": "message", "status": "completed"}}}} 
	if err := sendJSONEvent(conn, respDone); err != nil {
		return
	}

	log.Printf("Client %s: Finished streaming response (RespID: %s)", conn.RemoteAddr(), responseID)
}

func streamAudio(conn *websocket.Conn, responseID, itemID string, contentIndex int) {
	// This check is now effectively handled by the caller (streamResponse)
	// if appConfig.Mock.AudioWavPath == "" { ... } 
	file, err := os.Open(appConfig.Mock.AudioWavPath)
	if err != nil {
		log.Printf("Client %s: ERROR opening audio file %s: %v", conn.RemoteAddr(), appConfig.Mock.AudioWavPath, err)
		sendErrorEvent(conn, "audio_file_error", fmt.Sprintf("Failed to open audio file: %v", err))
		return
	}
	defer file.Close()

	// Skip WAV header (assume 44 bytes)
	header := make([]byte, 44)
	// Use io.ReadFull to ensure the whole header is read.
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
				return 
			}
		}

		if err == io.EOF {
			log.Printf("Client %s: Reached EOF for audio file %s", conn.RemoteAddr(), appConfig.Mock.AudioWavPath)
			break 
		}
		if err != nil {
			log.Printf("Client %s: ERROR reading WAV data from %s: %v", conn.RemoteAddr(), appConfig.Mock.AudioWavPath, err)
			sendErrorEvent(conn, "audio_file_error", fmt.Sprintf("Error reading audio data: %v", err))
			break 
		}
	}
	// The 'response.audio.done' event is now sent by the caller (streamResponse)
	// log.Printf("Client %s: Finished streaming audio chunks.", conn.RemoteAddr())
}

func streamTranscript(conn *websocket.Conn, responseID, itemID string, contentIndex int) {
	// This check is now effectively handled by the caller (streamResponse)
	// if appConfig.Mock.TranscriptText == "" { ... }

	words := strings.Fields(appConfig.Mock.TranscriptText)
	if len(words) == 0 { // Handle case where TranscriptText might be spaces only
		log.Printf("Client %s: Transcript text is empty after splitting into words. No transcript deltas to send.", conn.RemoteAddr())
		return
	}

	ticker := time.NewTicker(time.Duration(appConfig.Mock.ChunkIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	wordIndex := 0
	// fullSentTranscript := "" // Not strictly needed here anymore as 'response.text.done' gets full text from appConfig

	for range ticker.C {
		if wordIndex >= len(words) {
			break 
		}

		delta := words[wordIndex] + " "
		// fullSentTranscript += delta // Not needed for individual deltas

		transcriptDelta := map[string]interface{}{
			"type":          "response.text.delta",
			"event_id":      uuid.NewString(),
			"response_id":   responseID,
			"item_id":       itemID,
			"output_index":  0, 
			"content_index": contentIndex,
			"delta":         delta,
		}
		if err := sendJSONEvent(conn, transcriptDelta); err != nil {
			log.Printf("Client %s: Error sending transcript delta: %v", conn.RemoteAddr(), err)
			return 
		}
		wordIndex++
	}
	// The 'response.text.done' event is now sent by the caller (streamResponse)
	// log.Printf("Client %s: Finished streaming transcript chunks.", conn.RemoteAddr())
}

// --- Helper Functions ---

var wsWriteMutex sync.Mutex 

func sendJSONEvent(conn *websocket.Conn, event interface{}) error {
	wsWriteMutex.Lock()
	defer wsWriteMutex.Unlock()
	
	// Check if connection is still valid before writing
	if conn == nil {
		log.Printf("Attempted to write to a nil WebSocket connection.")
		return fmt.Errorf("websocket connection is nil")
	}
	// Example: Setting a write deadline (optional, adjust as needed)
	// if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
	//  log.Printf("Client %s: Error setting write deadline: %v", conn.RemoteAddr(), err)
	//  return err
	// }

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
	// Example: Clear the write deadline after successful write (if set)
	// if err := conn.SetWriteDeadline(time.Time{}); err != nil {
	//  log.Printf("Client %s: Error clearing write deadline: %v", conn.RemoteAddr(), err)
	//  // Don't necessarily return error here, as write was successful
	// }
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
