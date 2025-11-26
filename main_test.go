package main

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Helper function to load config for tests
func loadTestConfig(tb testing.TB) {
	// Create a temporary test config file
	configContent := `
server:
  port: 8080
mock:
  responseDelaySeconds: 0
  audioWavPath: "./mock_audio.wav"
  chunkIntervalMs: 50
  audioChunkSizeBytes: 1024
scenarios:
  - name: default
    events:
      - type: message
        delay_ms: 100
        text: "Default scenario text"
  - name: test_scenario
    events:
      - type: message
        delay_ms: 50
        text: "Test scenario text"
  - name: transcriptionTest
    events:
      - type: user_transcription
        delay_ms: 50
        text: "Hello, I would like to book a flight."
      - type: message
        delay_ms: 50
        text: "Acknowledged."
`
	err := os.WriteFile("test_config.yaml", []byte(configContent), 0644)
	if err != nil {
		tb.Fatalf("Failed to write test config: %v", err)
	}

	if _, err := loadConfiguration("test_config.yaml"); err != nil {
		tb.Fatalf("Failed to load test configuration: %v", err)
	}
}

func TestMain(m *testing.M) {
	// Setup: Create valid 24kHz PCM16 Mono WAV file
	f, err := os.Create("./mock_audio.wav")
	if err != nil {
		panic(err)
	}

	// Write minimal WAV header
	// RIFF (4) + Size (4) + WAVE (4)
	// fmt  (4) + Size (4) + AudioFormat(2) + NumChannels(2) + SampleRate(4) + ByteRate(4) + BlockAlign(2) + BitsPerSample(2)
	// data (4) + Size (4)

	header := make([]byte, 44)
	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], 36+100) // ChunkSize
	copy(header[8:12], []byte("WAVE"))
	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16)      // Subchunk1Size
	binary.LittleEndian.PutUint16(header[20:22], 1)       // AudioFormat (PCM)
	binary.LittleEndian.PutUint16(header[22:24], 1)       // NumChannels (Mono)
	binary.LittleEndian.PutUint32(header[24:28], 24000)   // SampleRate
	binary.LittleEndian.PutUint32(header[28:32], 24000*2) // ByteRate
	binary.LittleEndian.PutUint16(header[32:34], 2)       // BlockAlign
	binary.LittleEndian.PutUint16(header[34:36], 16)      // BitsPerSample
	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], 100) // Subchunk2Size

	f.Write(header)
	f.Write(make([]byte, 100)) // Dummy data
	f.Close()

	// Run tests
	exitCode := m.Run()

	// Teardown
	os.Remove("test_config.yaml")
	// os.Remove("./mock_audio.wav")

	os.Exit(exitCode)
}

func TestHandleCreateSession(t *testing.T) {
	loadTestConfig(t)

	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/realtime/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var sessionResponse struct {
		ID           string `json:"id"`
		ClientSecret struct {
			Value     string `json:"value"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"client_secret"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sessionResponse); err != nil {
		t.Fatalf("Failed to decode response body: %v", err)
	}

	if sessionResponse.ID == "" {
		t.Error("Expected 'id' field to be non-empty")
	}
}

func TestHandleWebSocket_DefaultScenario(t *testing.T) {
	loadTestConfig(t)

	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	// Read initial messages
	for i := 0; i < 2; i++ {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Error reading initial message: %v", err)
		}
	}

	// Send input audio to trigger scenario
	appendMessage := map[string]string{
		"type":  "input_audio_buffer.append",
		"audio": "AAA=",
	}
	if err := conn.WriteJSON(appendMessage); err != nil {
		t.Fatalf("Failed to send input_audio_buffer.append: %v", err)
	}

	// Expect response from default scenario
	// "Default scenario text"
	foundText := false

	// Set read deadline
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				t.Fatal("Timeout waiting for response")
			}
			t.Fatalf("Read error: %v", err)
		}

		var msg map[string]interface{}
		json.Unmarshal(message, &msg)

		if msg["type"] == "response.audio_transcript.delta" {
			if delta, ok := msg["delta"].(string); ok {
				if strings.Contains(delta, "Default") {
					foundText = true
				}
			}
		}
		if msg["type"] == "response.done" {
			if !foundText {
				t.Error("Did not receive expected text delta")
			}
			return
		}
	}
}

func TestHandleWebSocket_SpecificScenario(t *testing.T) {
	loadTestConfig(t)

	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	// Connect with scenario query param
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?scenario=test_scenario"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	// Read initial messages
	for i := 0; i < 2; i++ {
		conn.ReadMessage()
	}

	// Send input audio
	conn.WriteJSON(map[string]string{
		"type":  "input_audio_buffer.append",
		"audio": "AAA=",
	})

	// Expect response from test_scenario
	// "Test scenario text"
	foundText := false

	// Set read deadline
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				t.Fatal("Timeout waiting for response")
			}
			t.Fatalf("Read error: %v", err)
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg["type"] == "response.audio_transcript.delta" {
			if delta, ok := msg["delta"].(string); ok {
				if strings.Contains(delta, "Test") {
					foundText = true
				}
			}
		}
		if msg["type"] == "response.done" {
			if !foundText {
				t.Error("Did not receive expected text delta for specific scenario")
			}
			return
		}
	}
}

func TestHandleWebSocket_UserTranscription(t *testing.T) {
	loadTestConfig(t)

	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	// Connect with scenario query param
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?scenario=transcriptionTest"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	// Read initial messages
	for i := 0; i < 2; i++ {
		conn.ReadMessage()
	}

	// Send input audio to trigger scenario
	conn.WriteJSON(map[string]string{
		"type":  "input_audio_buffer.append",
		"audio": "AAA=",
	})

	// Expect user transcription event
	// Expect user transcription event sequence
	// 1. input_audio_buffer.committed
	// 2. conversation.item.created
	// 3. conversation.item.input_audio_transcription.completed

	foundCommitted := false
	foundItemCreated := false
	foundTranscription := false
	expectedTranscript := "Hello, I would like to book a flight."

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				t.Fatal("Timeout waiting for response")
			}
			t.Fatalf("Read error: %v", err)
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg["type"] == "input_audio_buffer.committed" {
			foundCommitted = true
		}
		if msg["type"] == "conversation.item.created" {
			if foundCommitted {
				foundItemCreated = true
			}
		}
		if msg["type"] == "conversation.item.input_audio_transcription.completed" {
			if transcript, ok := msg["transcript"].(string); ok {
				if transcript == expectedTranscript {
					foundTranscription = true
				}
			}
		}
		if msg["type"] == "response.done" {
			if !foundCommitted {
				t.Error("Did not receive input_audio_buffer.committed event")
			}
			if !foundItemCreated {
				t.Error("Did not receive conversation.item.created event")
			}
			if !foundTranscription {
				t.Errorf("Did not receive expected user transcription event. Expected '%s'", expectedTranscript)
			}
			return
		}
	}
}

func TestValidateWavFormat(t *testing.T) {
	// Create invalid WAV (wrong sample rate)
	f, _ := os.Create("invalid.wav")
	header := make([]byte, 44)
	copy(header[0:4], []byte("RIFF"))
	copy(header[8:12], []byte("WAVE"))
	binary.LittleEndian.PutUint16(header[20:22], 1)     // PCM
	binary.LittleEndian.PutUint16(header[22:24], 1)     // Mono
	binary.LittleEndian.PutUint32(header[24:28], 16000) // 16kHz (Invalid)
	binary.LittleEndian.PutUint16(header[34:36], 16)    // 16-bit
	f.Write(header)
	f.Close()
	defer os.Remove("invalid.wav")

	if err := validateWavFormat("invalid.wav"); err == nil {
		t.Error("Expected error for 16kHz WAV, got nil")
	}

	// Valid WAV is created in TestMain as ./mock_audio.wav
	if err := validateWavFormat("./mock_audio.wav"); err != nil {
		t.Errorf("Expected valid WAV to pass, got error: %v", err)
	}
}

func TestHandleWebSocket_FunctionCall(t *testing.T) {
	loadTestConfig(t)

	// Add a function call scenario to the config for this test
	configContent := `
server:
  port: 8080
mock:
  responseDelaySeconds: 0
  audioWavPath: "./mock_audio.wav"
  chunkIntervalMs: 50
  audioChunkSizeBytes: 1024
scenarios:
  - name: functionCallTest
    events:
      - type: function_call
        delay_ms: 50
        function_call:
          name: "get_weather"
          arguments: "{\"location\": \"San Francisco\"}"
`
	err := os.WriteFile("test_config_fc.yaml", []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}
	defer os.Remove("test_config_fc.yaml")

	if _, err := loadConfiguration("test_config_fc.yaml"); err != nil {
		t.Fatalf("Failed to load test configuration: %v", err)
	}

	router := setupRouter()
	server := httptest.NewServer(router)
	defer server.Close()

	// Connect with scenario query param
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?scenario=functionCallTest"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	// Read initial messages
	for i := 0; i < 2; i++ {
		conn.ReadMessage()
	}

	// Send trigger
	conn.WriteJSON(map[string]string{
		"type":  "input_audio_buffer.append",
		"audio": "AAA=",
	})

	// Expect sequence:
	// 1. response.created
	// 2. conversation.item.created
	// 3. response.output_item.added
	// ... args delta ...
	// ... args done ...
	// ... item done ...
	// ... response done ...

	foundResponseCreated := false
	foundConvItemCreated := false
	foundItemAdded := false
	foundArgs := false

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				t.Fatal("Timeout waiting for response")
			}
			t.Fatalf("Read error: %v", err)
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}
		t.Logf("Received event: %s", msg["type"])

		if msg["type"] == "response.created" {
			foundResponseCreated = true
			// Verify output field exists
			if resp, ok := msg["response"].(map[string]interface{}); ok {
				if _, ok := resp["output"]; !ok {
					t.Error("response.created missing 'output' field")
				}
			}
		}
		if msg["type"] == "conversation.item.created" {
			if foundResponseCreated {
				foundConvItemCreated = true
			} else {
				t.Error("Received conversation.item.created BEFORE response.created")
			}
		}
		if msg["type"] == "response.output_item.added" {
			if foundConvItemCreated {
				foundItemAdded = true
			} else {
				t.Error("Received response.output_item.added BEFORE conversation.item.created")
			}
		}
		if msg["type"] == "response.function_call_arguments.done" {
			foundArgs = true
		}

		if msg["type"] == "response.done" {
			if !foundResponseCreated {
				t.Error("Did not receive response.created")
			}
			if !foundConvItemCreated {
				t.Error("Did not receive conversation.item.created")
			}
			if !foundItemAdded {
				t.Error("Did not receive response.output_item.added")
			}
			if !foundArgs {
				t.Error("Did not receive function call arguments")
			}
			return
		}
	}
}
