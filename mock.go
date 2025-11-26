package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
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
)

// --- Mock Mode Logic ---

func handleMockWebSocket(w http.ResponseWriter, r *http.Request) {
	// 1. Determine Scenario or Replay
	scenarioName := r.URL.Query().Get("scenario")
	replaySessionName := r.URL.Query().Get("replaySession")

	var selectedScenario Scenario
	var isReplay bool
	var replayFilePath string

	found := false

	// 1. Check for Replay
	if replaySessionName != "" {
		recordingDir := appConfig.Proxy.RecordingPath
		if recordingDir == "" {
			recordingDir = "recordings"
		}

		// Try exact match or with .ndjson extension
		possiblePaths := []string{
			filepath.Join(recordingDir, replaySessionName),
			filepath.Join(recordingDir, replaySessionName+".ndjson"),
		}

		for _, path := range possiblePaths {
			if _, err := os.Stat(path); err == nil {
				replayFilePath = path
				isReplay = true
				found = true
				log.Printf("Found recording for replay: %s", path)
				break
			}
		}
		if !found {
			log.Printf("Replay session '%s' not found in %s", replaySessionName, recordingDir)
		}
	}

	// 2. Check Config Scenarios (if not a replay)
	if !found && scenarioName != "" {
		for _, s := range appConfig.Scenarios {
			if s.Name == scenarioName {
				selectedScenario = s
				found = true
				break
			}
		}
	}

	if !found && len(appConfig.Scenarios) > 0 {
		// If neither found, default to first scenario (unless replay was explicitly requested but failed?)
		// If replay was requested but not found, we probably shouldn't fallback to default scenario silently?
		// But for now let's keep the fallback behavior but maybe log it.
		if replaySessionName != "" {
			log.Printf("Replay session not found. Falling back to default scenario.")
		} else if scenarioName != "" {
			log.Printf("Scenario '%s' not found. Falling back to default scenario.", scenarioName)
		}

		selectedScenario = appConfig.Scenarios[0]
		log.Printf("Using default scenario: %s", selectedScenario.Name)
	} else if !found {
		log.Printf("No scenarios available to run.")
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	safeConn := &SafeWebSocket{Conn: conn}
	defer safeConn.Close()

	if isReplay {
		log.Printf("WebSocket client connected: %s. Replaying: %s", safeConn.RemoteAddr(), replayFilePath)
	} else {
		log.Printf("WebSocket client connected: %s. Scenario: %s", safeConn.RemoteAddr(), selectedScenario.Name)
	}

	// --- Send Welcome Messages (SessionCreated, ConversationCreated) ---
	// Note: In a real replay, these might be in the log, but usually the client expects them immediately.
	// If the log contains them, we might duplicate them.
	// However, the proxy records "inbound" messages, which includes session.created if it was sent by OpenAI.
	// So for replay, we might NOT want to send these manually if they are in the file.
	// But let's stick to the standard flow: Client connects -> Server sends Hello.
	// If the recording starts with session.created, we might send it twice.
	// Let's assume we send standard hello, then replay the rest.

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
	if err := sendJSONEvent(safeConn, sessionCreated); err != nil {
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
	if err := sendJSONEvent(safeConn, convCreated); err != nil {
		return
	}

	// --- Simple Client State ---
	var scenarioOnce sync.Once
	audioReceived := false

	// --- Read Loop ---
	for {
		messageType, message, err := safeConn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %s read error: %v", safeConn.RemoteAddr(), err)
			} else {
				log.Printf("Client %s disconnected: %v", safeConn.RemoteAddr(), err)
			}
			break // Exit loop on error or close
		}

		if messageType == websocket.TextMessage {
			var base BaseEvent
			if err := json.Unmarshal(message, &base); err == nil {
				log.Printf("Client %s received event: %s", safeConn.RemoteAddr(), base.Type)

				if base.Type == "input_audio_buffer.append" || base.Type == "response.create" {
					if !audioReceived {
						audioReceived = true
						log.Printf("Client %s: Trigger event received (%s). Starting response.", safeConn.RemoteAddr(), base.Type)
						scenarioOnce.Do(func() {
							go func() {
								// Delay before starting response (only for scenarios, not replays)
								if !isReplay && appConfig.Mock.ResponseDelaySeconds > 0 {
									time.Sleep(time.Duration(appConfig.Mock.ResponseDelaySeconds) * time.Second)
								}

								if isReplay {
									runReplay(safeConn, replayFilePath)
								} else {
									runScenario(safeConn, selectedScenario, sessionID)
								}
							}()
						})
					}
				}
			} else {
				log.Printf("Client %s received non-JSON text message or parse error: %v", safeConn.RemoteAddr(), err)
			}
		} else if messageType == websocket.BinaryMessage {
			log.Printf("Client %s received binary message (%d bytes) - treating as audio", safeConn.RemoteAddr(), len(message))
			if !audioReceived {
				audioReceived = true
				log.Printf("Client %s: First binary audio received. Starting response.", safeConn.RemoteAddr())
				scenarioOnce.Do(func() {
					go func() {
						if appConfig.Mock.ResponseDelaySeconds > 0 {
							time.Sleep(time.Duration(appConfig.Mock.ResponseDelaySeconds) * time.Second)
						}

						if isReplay {
							runReplay(safeConn, replayFilePath)
						} else {
							runScenario(safeConn, selectedScenario, sessionID)
						}
					}()
				})
			}
		}
	}
}

// --- Replay Logic ---

func runReplay(conn *SafeWebSocket, filePath string) {
	log.Printf("Starting replay from: %s", filePath)

	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Failed to open replay file: %v", err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines (audio chunks can be large)
	const maxCapacity = 1024 * 1024 * 10 // 10MB
	buf := make([]byte, maxCapacity)
	scanner.Buffer(buf, maxCapacity)

	var lastTimestamp int64
	firstEvent := true

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event RecordedEvent
		if err := json.Unmarshal(line, &event); err != nil {
			log.Printf("Error parsing replay line: %v. Skipping.", err)
			continue
		}

		// Calculate delay
		if firstEvent {
			lastTimestamp = event.Timestamp
			firstEvent = false
		}

		delay := event.Timestamp - lastTimestamp
		if delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
		lastTimestamp = event.Timestamp

		// Send raw data
		if err := conn.WriteMessage(websocket.TextMessage, event.Data); err != nil {
			log.Printf("Error sending replay message: %v", err)
			return
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading replay file: %v", err)
	}

	log.Printf("Replay completed: %s", filePath)
}

// --- Scenario Execution Logic ---

func runScenario(conn *SafeWebSocket, scenario Scenario, sessionID string) {
	log.Printf("Starting scenario execution: %s", scenario.Name)

	for i, event := range scenario.Events {
		// 1. Wait for delay
		if event.DelayMs > 0 {
			time.Sleep(time.Duration(event.DelayMs) * time.Millisecond)
		}

		log.Printf("Executing event %d/%d (Type: %s)", i+1, len(scenario.Events), event.Type)

		// 2. Execute Event
		switch event.Type {
		case "message":
			streamMessageResponse(conn, event, sessionID)
		case "function_call":
			sendFunctionCall(conn, event, sessionID)
		case "user_transcription":
			sendUserTranscription(conn, event, sessionID)
		default:
			log.Printf("Unknown event type: %s", event.Type)
		}
	}
	log.Printf("Scenario execution completed: %s", scenario.Name)
}

func streamMessageResponse(conn *SafeWebSocket, event Event, sessionID string) {
	responseID := "mock-resp-" + uuid.NewString()
	itemID := "mock-item-" + uuid.NewString()

	// 1. response.created
	respCreated := map[string]interface{}{
		"type":     "response.created",
		"event_id": uuid.NewString(),
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "realtime.response",
			"status": "in_progress",
			"output": []interface{}{},
		},
	}
	if err := sendJSONEvent(conn, respCreated); err != nil {
		return
	}

	// 2. response.output_item.added
	itemAdded := map[string]interface{}{
		"type":         "response.output_item.added",
		"event_id":     uuid.NewString(),
		"response_id":  responseID,
		"output_index": 0,
		"item": map[string]interface{}{
			"id":      itemID,
			"object":  "realtime.item",
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []interface{}{},
		},
	}
	if err := sendJSONEvent(conn, itemAdded); err != nil {
		return
	}

	// 3. conversation.item.created (Crucial for client to know about the item)
	convItemCreated := map[string]interface{}{
		"type":             "conversation.item.created",
		"event_id":         uuid.NewString(),
		"previous_item_id": nil, // In a real scenario, this would be the last item ID
		"item": map[string]interface{}{
			"id":      itemID,
			"object":  "realtime.item",
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []interface{}{},
		},
	}
	if err := sendJSONEvent(conn, convItemCreated); err != nil {
		return
	}

	// Start streaming content
	var wg sync.WaitGroup

	// We will use a single content part for Audio + Transcript
	// In the Realtime API, audio response usually comes as a single content part with type="audio"
	// which contains both "audio" (base64) and "transcript" (text) fields updates.

	// response.content_part.added
	partAdded := map[string]interface{}{
		"type":          "response.content_part.added",
		"event_id":      uuid.NewString(),
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"part": map[string]interface{}{
			"type":       "audio",
			"transcript": "",
		},
	}
	if err := sendJSONEvent(conn, partAdded); err != nil {
		return
	}

	// Stream Audio and Transcript concurrently
	if appConfig.Mock.AudioWavPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamAudio(conn, responseID, itemID, 0)
		}()
	}

	if event.Text != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamTranscript(conn, responseID, itemID, 0, event.Text)
		}()
	}

	wg.Wait()

	// response.content_part.done
	partDone := map[string]interface{}{
		"type":          "response.content_part.done",
		"event_id":      uuid.NewString(),
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"part": map[string]interface{}{
			"type":       "audio",
			"transcript": event.Text,
		},
	}
	if err := sendJSONEvent(conn, partDone); err != nil {
		return
	}

	// response.output_item.done
	itemDoneContent := []interface{}{
		map[string]interface{}{
			"type":       "audio",
			"transcript": event.Text,
			// "audio": "..." // We don't include full audio in done event usually to save bandwidth in logs, but API might
		},
	}

	itemDone := map[string]interface{}{
		"type":         "response.output_item.done",
		"event_id":     uuid.NewString(),
		"response_id":  responseID,
		"output_index": 0,
		"item": map[string]interface{}{
			"id":      itemID,
			"object":  "realtime.item",
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": itemDoneContent,
		},
	}
	if err := sendJSONEvent(conn, itemDone); err != nil {
		return
	}

	// response.done
	respDone := map[string]interface{}{
		"type":     "response.done",
		"event_id": uuid.NewString(),
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "realtime.response",
			"status": "completed",
			"output": []interface{}{
				map[string]interface{}{
					"id":     itemID,
					"object": "realtime.item",
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []interface{}{
						map[string]interface{}{
							"type":       "audio",
							"transcript": event.Text,
						},
					},
				},
			},
		},
	}
	sendJSONEvent(conn, respDone)
}

func sendFunctionCall(conn *SafeWebSocket, event Event, sessionID string) {
	if event.FunctionCall == nil {
		log.Printf("Error: FunctionCall definition missing for event")
		return
	}

	responseID := "mock-resp-fc-" + uuid.NewString()
	itemID := "mock-item-fc-" + uuid.NewString()
	callID := "call_" + uuid.NewString()

	// 1. response.created
	respCreated := map[string]interface{}{
		"type":     "response.created",
		"event_id": uuid.NewString(),
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "realtime.response",
			"status": "in_progress",
			"output": []interface{}{},
		},
	}
	if err := sendJSONEvent(conn, respCreated); err != nil {
		return
	}

	// 2. conversation.item.created
	itemCreated := map[string]interface{}{
		"type":             "conversation.item.created",
		"event_id":         uuid.NewString(),
		"previous_item_id": nil,
		"item": map[string]interface{}{
			"id":        itemID,
			"object":    "realtime.item",
			"type":      "function_call",
			"status":    "in_progress",
			"name":      event.FunctionCall.Name,
			"call_id":   callID,
			"arguments": "",
		},
	}
	if err := sendJSONEvent(conn, itemCreated); err != nil {
		return
	}

	// response.output_item.added
	itemAdded := map[string]interface{}{
		"type":         "response.output_item.added",
		"event_id":     uuid.NewString(),
		"response_id":  responseID,
		"output_index": 0,
		"item": map[string]interface{}{
			"id":        itemID,
			"object":    "realtime.item",
			"type":      "function_call",
			"status":    "in_progress",
			"name":      event.FunctionCall.Name,
			"call_id":   callID,
			"arguments": "", // Starts empty
		},
	}
	if err := sendJSONEvent(conn, itemAdded); err != nil {
		return
	}

	// Stream arguments (simulate streaming by sending chunks)
	args := event.FunctionCall.Arguments
	chunkSize := 10
	for i := 0; i < len(args); i += chunkSize {
		end := i + chunkSize
		if end > len(args) {
			end = len(args)
		}
		chunk := args[i:end]

		delta := map[string]interface{}{
			"type":         "response.function_call_arguments.delta",
			"event_id":     uuid.NewString(),
			"response_id":  responseID,
			"item_id":      itemID,
			"output_index": 0,
			"call_id":      callID,
			"delta":        chunk,
		}
		if err := sendJSONEvent(conn, delta); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond) // Small delay for realism
	}

	// response.function_call_arguments.done
	argsDone := map[string]interface{}{
		"type":         "response.function_call_arguments.done",
		"event_id":     uuid.NewString(),
		"response_id":  responseID,
		"item_id":      itemID,
		"output_index": 0,
		"call_id":      callID,
		"arguments":    args,
	}
	if err := sendJSONEvent(conn, argsDone); err != nil {
		return
	}

	// response.done
	respDone := map[string]interface{}{
		"type":     "response.done",
		"event_id": uuid.NewString(),
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "realtime.response",
			"status": "completed",
			"output": []interface{}{
				map[string]interface{}{
					"id":        itemID,
					"object":    "realtime.item",
					"type":      "function_call",
					"status":    "completed",
					"name":      event.FunctionCall.Name,
					"call_id":   callID,
					"arguments": args,
				},
			},
		},
	}
	sendJSONEvent(conn, respDone)
}

func sendUserTranscription(conn *SafeWebSocket, event Event, sessionID string) {
	itemID := "mock-item-trans-" + uuid.NewString()

	// 1. input_audio_buffer.committed
	committed := map[string]interface{}{
		"type":             "input_audio_buffer.committed",
		"event_id":         uuid.NewString(),
		"previous_item_id": nil,
		"item_id":          itemID,
	}
	if err := sendJSONEvent(conn, committed); err != nil {
		log.Printf("Failed to send input_audio_buffer.committed: %v", err)
		return
	}

	// 2. conversation.item.created
	itemCreated := map[string]interface{}{
		"type":             "conversation.item.created",
		"event_id":         uuid.NewString(),
		"previous_item_id": nil,
		"item": map[string]interface{}{
			"id":     itemID,
			"object": "realtime.item",
			"type":   "message",
			"status": "completed",
			"role":   "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":       "input_audio",
					"transcript": nil, // Transcript comes later in the event
				},
			},
		},
	}
	if err := sendJSONEvent(conn, itemCreated); err != nil {
		log.Printf("Failed to send conversation.item.created: %v", err)
		return
	}

	// 3. conversation.item.input_audio_transcription.completed
	transcriptionCompleted := map[string]interface{}{
		"type":          "conversation.item.input_audio_transcription.completed",
		"event_id":      uuid.NewString(),
		"item_id":       itemID,
		"content_index": 0,
		"transcript":    event.Text,
	}
	if err := sendJSONEvent(conn, transcriptionCompleted); err != nil {
		log.Printf("Failed to send user transcription: %v", err)
	}
}

func streamAudio(conn *SafeWebSocket, responseID, itemID string, contentIndex int) {
	file, err := os.Open(appConfig.Mock.AudioWavPath)
	if err != nil {
		log.Printf("Client %s: ERROR opening audio file %s: %v", conn.RemoteAddr(), appConfig.Mock.AudioWavPath, err)
		return
	}
	defer file.Close()

	// Skip WAV header (assume 44 bytes)
	header := make([]byte, 44)
	if _, err = io.ReadFull(file, header); err != nil {
		log.Printf("Client %s: ERROR reading WAV header: %v", conn.RemoteAddr(), err)
		return
	}

	buffer := make([]byte, appConfig.Mock.AudioChunkSizeBytes)
	ticker := time.NewTicker(time.Duration(appConfig.Mock.ChunkIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Note: response.content_part.added is now sent in streamMessageResponse

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
				"output_index":  0,
				"content_index": contentIndex,
				"delta":         encodedData,
			}
			if err := sendJSONEvent(conn, audioDelta); err != nil {
				return
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Error reading audio: %v", err)
			break
		}
	}

	// response.output_audio.done
	audioDone := map[string]interface{}{
		"type":          "response.output_audio.done",
		"event_id":      uuid.NewString(),
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": contentIndex,
	}
	sendJSONEvent(conn, audioDone)
}

func streamTranscript(conn *SafeWebSocket, responseID, itemID string, contentIndex int, text string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return
	}

	ticker := time.NewTicker(time.Duration(appConfig.Mock.ChunkIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Note: response.content_part.added is now sent in streamMessageResponse

	wordIndex := 0
	for range ticker.C {
		if wordIndex >= len(words) {
			break
		}

		delta := words[wordIndex] + " "
		transcriptDelta := map[string]interface{}{
			"type":          "response.audio_transcript.delta",
			"event_id":      uuid.NewString(),
			"response_id":   responseID,
			"item_id":       itemID,
			"output_index":  0,
			"content_index": contentIndex,
			"delta":         delta,
		}
		if err := sendJSONEvent(conn, transcriptDelta); err != nil {
			return
		}
		wordIndex++
	}

	// response.output_audio_transcript.done
	transcriptDone := map[string]interface{}{
		"type":          "response.output_audio_transcript.done",
		"event_id":      uuid.NewString(),
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": contentIndex,
		"transcript":    text,
	}
	sendJSONEvent(conn, transcriptDone)
}
