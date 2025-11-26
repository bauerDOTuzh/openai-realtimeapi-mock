# Simple OpenAI Realtime API WebSocket Mock

This Go application provides a **basic mock server** for the WebSocket portion of the OpenAI Realtime API. It is designed for testing scenarios where you need a server that mimics the basic interaction flow, including configurable delays, function calls, and user transcriptions.

**This is a highly simplified mock and does NOT replicate the full functionality or complexity of the real OpenAI API.**

## Features

*   **Scenario-Based Execution:** Define multiple named scenarios in `config.yaml` and select them at runtime.
*   **Configurable Delays:** Simulate network or processing latency with global response delays and per-event delays.
*   **Event Types:**
    *   `message`: Streams back audio and text (transcript).
    *   `function_call`: Simulates OpenAI's function call events (`response.function_call_arguments.delta`, etc.).
    *   `user_transcription`: Simulates the server acknowledging user speech with a transcription event.
*   **Audio Streaming:** Streams 24kHz PCM16 audio from a WAV file.
*   **WebSocket & HTTP:** Listens for WebSocket connections and provides a session creation endpoint.

## Prerequisites

*   **Go:** Version 1.18 or later recommended.
*   **A WAV Audio File:** You need a `.wav` file containing the audio you want the mock to stream back.
    *   **Format:** **MUST be 16-bit PCM, single-channel (mono), 24kHz sample rate**. The server validates this format on startup.
*   **WebSocket Client:** A tool like Postman, `wscat`, or a custom client application.

## Configuration (`config.yaml`)

Create a `config.yaml` file in the same directory as the executable.

```yaml
server:
  port: 8080

mock:
  # Delay in seconds after receiving the *first* audio chunk before responding
  responseDelaySeconds: 2
  # Path to the WAV file to play back (MUST be 24kHz PCM16 Mono)
  audioWavPath: "./mock_audio.wav"
  # How often to send audio/transcript chunks (milliseconds)
  chunkIntervalMs: 100
  # How many audio data bytes per chunk to send
  audioChunkSizeBytes: 4096

scenarios:
  - name: default
    events:
      - type: message
        delay_ms: 1000
        text: "This is the default response."

  - name: booking_flow
    events:
      - type: user_transcription
        delay_ms: 500
        text: "I want to book a flight."
      - type: message
        delay_ms: 1000
        text: "Sure, where are you flying to?"
      - type: function_call
        delay_ms: 2000
        function_call:
          name: "search_flights"
          arguments: "{\"destination\": \"London\"}"
```

## Usage

### 1. Start the Server
```bash
go run main.go --config config.yaml
```

### 2. Connect via WebSocket
You can select a specific scenario using the `scenario` query parameter.

*   **Default Scenario:** `ws://localhost:8080/v1/realtime`
*   **Specific Scenario:** `ws://localhost:8080/v1/realtime?scenario=booking_flow`

### 3. Trigger the Interaction
Send a JSON message with `type: input_audio_buffer.append` and some base64 audio data to start the interaction.

```json
{
  "type": "input_audio_buffer.append",
  "audio": "UklGRi..." // Base64 encoded audio
}
```

The server will wait for `responseDelaySeconds` and then execute the events defined in the selected scenario.

## Proxy Mode & Recording

The mock service can act as a proxy to the real OpenAI Realtime API. In this mode, it forwards all traffic between the client and OpenAI, and **records the session** to an NDJSON file.

### Enable Proxy Mode
Update `config.yaml`:
```yaml
mode: "proxy"
proxy:
  url: "wss://api.openai.com/v1/realtime"
  recordingPath: "./recordings"
  model: "gpt-4o-mini-realtime-preview-2024-12-17"
```
Ensure `OPENAI_API_KEY` is set in your environment.

### Recording
When a client connects in proxy mode, a new file is created in `recordingPath` (e.g., `recordings/2025-11-26_14-30-00.ndjson`). This file contains timestamped events from the session.

## Replay a Session

You can replay a recorded session to simulate the exact timing and data of a real interaction.

1.  Set `mode: "mock"` in `config.yaml`.
2.  Connect to the WebSocket with the `replaySession` parameter pointing to the recording filename.

```
ws://localhost:8080/v1/realtime?replaySession=2025-11-26_14-30-00.ndjson
```

The mock service will:
1.  Locate the file in the `recordings` directory.
2.  Replay the events with the **exact delays** as they occurred in the original session.

## Docker Usage

### Build
```bash
docker build -t openai-realtime-mock .
```

### Run
```bash
docker run -d -p 8080:8080 -v $(pwd)/config.yaml:/app/config.yaml -v $(pwd)/mock_audio.wav:/app/mock_audio.wav -v $(pwd)/recordings:/app/recordings openai-realtime-mock
```

## Alternative testing method
- use commit 6ea4dba795fee868c60ea9e8e7eba7469974b3e9 from openai realtime console and replace in file (after npm i) `node_modules/@openai/realtime-api-beta/lib/api.js:117` websocket to the desired one eg `ws://localhost:8085/v1/realtime?model=gpt-realtime-mini&scenario=complex_conversation`

or use `ws://localhost:8085/v1/realtime` and start your mock in proxy mode, 
then a new recording is created and this can be then accessed after changing to proxy and to ws://localhost:8085/v1/realtime?model=gpt-realtime-mini&replaySession=2025-11-26_13-15-49.ndjson