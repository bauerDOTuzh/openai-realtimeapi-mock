package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Helper function to load config for tests
// This function assumes `loadConfig` from main.go is accessible
// and `appConfig` is a package-level variable that `loadConfig` sets.
func loadTestConfig(tb testing.TB) {
	if err := loadConfig("test_config.yaml"); err != nil {
		tb.Fatalf("Failed to load test configuration: %v", err)
	}
}

func TestMain(m *testing.M) {
	// Setup: Load test configuration
	// We don't have a testing.TB here, so direct call and panic.
	if err := loadConfig("test_config.yaml"); err != nil {
		log.Fatalf("Failed to load test configuration in TestMain: %v", err)
	}

	// Ensure mock_audio.wav exists as per appConfig.Mock.AudioWavPath
	// This check helps catch configuration/environment issues early.
	if _, err := os.Stat(appConfig.Mock.AudioWavPath); os.IsNotExist(err) {
		log.Fatalf("Mock audio file not found at path: %s. Please ensure it exists.", appConfig.Mock.AudioWavPath)
	} else if err != nil {
		log.Fatalf("Error checking mock audio file %s: %v", appConfig.Mock.AudioWavPath, err)
	}

	// Run tests
	exitCode := m.Run()

	// Teardown (if any)

	os.Exit(exitCode)
}

func TestHandleCreateSession(t *testing.T) {
	// Ensure test config is loaded for this test instance if appConfig could be reset
	// loadTestConfig(t) // Usually TestMain is enough

	// Initialize the router and server
	// setupRouter should use the appConfig loaded in TestMain
	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	// Make a POST request to /v1/realtime/sessions
	resp, err := http.Post(server.URL+"/v1/realtime/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Verify status code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}

	// Verify response body
	var sessionResponse struct {
		ID           string `json:"id"`
		ClientSecret struct {
			Value     string `json:"value"`
			ExpiresAt int64  `json:"expires_at"` // Assuming timestamp in milliseconds
		} `json:"client_secret"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sessionResponse); err != nil {
		t.Fatalf("Failed to decode response body: %v", err)
	}

	// Verify fields in the response
	if sessionResponse.ID == "" {
		t.Error("Expected 'id' field to be non-empty")
	}
	if sessionResponse.ClientSecret.Value == "" {
		t.Error("Expected 'client_secret.value' field to be non-empty")
	}
	if sessionResponse.ClientSecret.ExpiresAt == 0 {
		t.Error("Expected 'client_secret.expires_at' field to be non-zero")
	}
	// Check if expires_at is in the future (rough check, assuming ms)
	// Convert server's ExpiresAt (milliseconds) to time.Time
	expiresAtTime := time.Unix(0, sessionResponse.ClientSecret.ExpiresAt*int64(time.Millisecond))
	if expiresAtTime.Before(time.Now()) {
		t.Errorf("Expected 'client_secret.expires_at' (%v) to be in the future", expiresAtTime)
	}
}

func TestHandleWebSocket(t *testing.T) {
	// loadTestConfig(t) // Usually TestMain is enough

	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	t.Logf("Connecting to WebSocket URL: %s", wsURL)

	// Connect to the WebSocket server
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	initialMessagesReceived := make(chan bool, 1) // Buffered to prevent goroutine leak if test fails early
	sessionCreatedReceived := false
	conversationCreatedReceived := false

	// Goroutine to read messages from the WebSocket for initial phase
	go func() {
		defer func() {
			// Only close if we are responsible for signaling
			if !(sessionCreatedReceived && conversationCreatedReceived) {
				// If we exit early due to error or timeout, ensure the channel is not left unclosed
				// Or rely on test timeout. For simplicity, let's assume test timeout handles this.
			}
		}()
		for i := 0; i < 2; i++ { // Expecting two initial messages
			_, message, err := conn.ReadMessage()
			if err != nil {
				// If connection closes unexpectedly, log and let the select timeout handle it
				t.Logf("Error reading initial message: %v", err)
				return
			}
			t.Logf("Received initial message: %s", string(message))

			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err != nil {
				t.Logf("Failed to unmarshal initial message: %v", err)
				continue
			}

			msgType, _ := msg["type"].(string)
			if msgType == "session.created" {
				sessionCreatedReceived = true
				t.Log("session.created received")
			}
			if msgType == "conversation.created" {
				conversationCreatedReceived = true
				t.Log("conversation.created received")
			}

			if sessionCreatedReceived && conversationCreatedReceived {
				initialMessagesReceived <- true
				return
			}
		}
	}()

	select {
	case <-initialMessagesReceived:
		t.Log("Successfully received initial 'session.created' and 'conversation.created' messages.")
	case <-time.After(5 * time.Second): // Timeout for initial messages
		t.Fatal("Timed out waiting for initial 'session.created' and 'conversation.created' messages.")
	}

	// Send input_audio_buffer.append
	appendMessage := map[string]string{
		"type":  "input_audio_buffer.append",
		"audio": "AAA=", // Base64 encoded dummy audio (single padding for 2 bytes of data)
	}
	if err := conn.WriteJSON(appendMessage); err != nil {
		t.Fatalf("Failed to send input_audio_buffer.append: %v", err)
	}
	t.Log("Sent input_audio_buffer.append")

	expectedMessageTypes := map[string]bool{
		"response.created":     false,
		"response.audio.delta": false,
		"response.text.delta":  false,
		"response.done":        false,
	}
	var receivedTextDeltas []string
	streamingMessagesDone := make(chan bool, 1) // Buffered

	// Goroutine to read streaming messages
	go func() {
		defer func() { streamingMessagesDone <- true }()
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					t.Log("WebSocket closed normally during streaming.")
				} else {
					// Don't call t.Errorf from a goroutine that might outlive the test if not handled well.
					// Log instead, and let the main test goroutine assert conditions.
					log.Printf("Error reading streaming message: %v", err)
				}
				return // Exit goroutine on any error or closure
			}
			t.Logf("Received streaming message: %s", string(message))

			var msg map[string]interface{}
			if errUnmarshal := json.Unmarshal(message, &msg); errUnmarshal != nil {
				log.Printf("Failed to unmarshal streaming message: %v", errUnmarshal)
				continue
			}

			msgType, _ := msg["type"].(string)

			if _, exists := expectedMessageTypes[msgType]; exists {
				if !expectedMessageTypes[msgType] { // Mark first occurrence
					expectedMessageTypes[msgType] = true
					t.Logf("Received expected message type for the first time: %s", msgType)
				}

				if msgType == "response.audio.delta" {
					delta, ok := msg["delta"].(string)
					if !ok || delta == "" {
						// Use t.Errorf directly here as this goroutine's lifecycle is tied to this test function
						// by the streamingMessagesDone channel and timeout.
						t.Errorf("'response.audio.delta' should have a non-empty 'delta' string: %v", msg)
					}
				}
				if msgType == "response.text.delta" {
					delta, ok := msg["delta"].(string)
					if !ok || delta == "" {
						t.Errorf("'response.text.delta' should have a non-empty 'delta' string: %v", msg)
					}
					receivedTextDeltas = append(receivedTextDeltas, delta)
				}
			}

			if msgType == "response.done" {
				expectedMessageTypes["response.done"] = true // Ensure it's marked if loop terminates early
				t.Log("response.done received. Finishing streaming message collection.")
				return // Stop this goroutine after response.done
			}
		}
	}()

	// Wait for streaming messages or timeout
	timeoutDuration := time.Duration(appConfig.Mock.ResponseDelaySeconds+5) * time.Second
	select {
	case <-streamingMessagesDone:
		t.Log("Streaming message checking completed.")
	case <-time.After(timeoutDuration):
		t.Fatalf("Timed out waiting for streaming messages after %v.", timeoutDuration)
	}

	// Verify all expected message types were received at least once
	allExpectedReceived := true
	for msgType, received := range expectedMessageTypes {
		if !received {
			// This check is problematic if mock_audio.wav is empty, as deltas might not be sent.
			// The task implies mock_audio.wav is valid and should produce output.
			// If TranscriptText is empty, text.delta might not be sent.
			if (msgType == "response.text.delta" || msgType == "response.audio.delta") && appConfig.Mock.TranscriptText == "" && isMockAudioEffectivelyEmpty(t) {
				t.Logf("Skipping check for %s as transcript is empty and audio might be minimal.", msgType)
				continue
			}
			t.Errorf("Did not receive expected message type: %s. Received states: %v", msgType, expectedMessageTypes)
			allExpectedReceived = false
		}
	}
	if allExpectedReceived {
		t.Log("All expected streaming message types were received.")
	}


	// Verify concatenated text deltas
	concatenatedText := strings.Join(receivedTextDeltas, "")
	t.Logf("Concatenated text: \"%s\"", concatenatedText)
	mockTranscript := appConfig.Mock.TranscriptText

	if mockTranscript != "" {
		if concatenatedText == "" {
			t.Errorf("Expected to receive text deltas for mock transcript \"%s\", but got none.", mockTranscript)
		}
		// For this mock setup, we expect the full transcript.
		// Depending on chunking, it might not be an exact match if the server logic is complex.
		// The prompt says "verify against a known part", but here full match is more robust for a simple mock.
		if concatenatedText != mockTranscript {
			t.Errorf("Concatenated text \"%s\" does not exactly match mock transcript \"%s\".", concatenatedText, mockTranscript)
		} else {
			t.Logf("Successfully matched concatenated text with mock transcript: \"%s\"", mockTranscript)
		}
	} else {
		if concatenatedText != "" {
			t.Errorf("Expected no text deltas as mock transcript is empty, but got: \"%s\"", concatenatedText)
		} else {
			t.Log("Correctly received no text deltas as mock transcript is empty.")
		}
	}
}

// isMockAudioEffectivelyEmpty is a helper to check if the mock audio is minimal (e.g., just WAV header)
// This is a placeholder for a more robust check if needed.
// For now, we assume if TranscriptText is empty, audio might also produce no meaningful deltas.
func isMockAudioEffectivelyEmpty(t *testing.T) bool {
	t.Helper()
	info, err := os.Stat(appConfig.Mock.AudioWavPath)
	if err != nil {
		t.Logf("Could not stat mock audio file: %v", err)
		return true // Assume empty or problematic if cannot stat
	}
	// A WAV file with only a header is typically 44 bytes.
	// If it's very small, it might not produce deltas.
	return info.Size() < 100 // Arbitrary small size
}
