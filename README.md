# Simple OpenAI Realtime API WebSocket Mock

This Go application provides a **basic mock server** for the WebSocket portion of the OpenAI Realtime API. It is designed for simple testing scenarios where you need a server that mimics the basic interaction flow: connect, receive some audio indication, wait for a delay, and then stream back a pre-defined audio file and transcript.

**This is a highly simplified mock and does NOT replicate the full functionality or complexity of the real OpenAI API.**

## What it Does

*   **Listens for WebSocket connections** on a configurable  port (defaults to `ws://localhost:8080/v1/realtime`).
*   Provides a minimal **HTTP endpoint** (`/v1/realtime/sessions`) that returns a fake session object with an ephemeral token, primarily to satisfy clients that need to call this before establishing a WebSocket connection.
*   Upon WebSocket connection, sends basic **welcome messages** (`session.created`, `conversation.created`).
*   **Detects the *first* incoming message** that signifies audio input (either a JSON message with `type: "input_audio_buffer.append"` or any binary message).
*   After detecting the first audio input, it **starts a configurable delay timer**.
*   When the timer fires, it **streams back audio data** chunk by chunk (`response.audio.delta`) by reading from a specified WAV file.
*   Simultaneously, it **streams back a pre-defined transcript** chunk by chunk (`response.text.delta` events).
*   Sends basic **response lifecycle events** (`response.created`, `*.added`, `*.done`) around the streaming process.
*   Configuration is managed through a `config.yaml` file.

## What it Does NOT Do

*   **No WebRTC Support:** Only mocks the WebSocket connection method.
*   **No Real Audio Processing:** It doesn't listen to, analyze, or transcribe the incoming audio data. The Base64 audio payload in `input_audio_buffer.append` is ignored.
*   **No Voice Activity Detection (VAD):** The response is triggered by the *first* audio message received, not by detecting speech pauses.
*   **No Actual AI/LLM Interaction:** The transcript and audio playback are pre-defined in the config/WAV file.
*   **Limited Event Handling:** Only really acts upon the first audio message. Other client events are mostly ignored or logged.
*   **No Authentication:** Doesn't validate API keys or tokens.
*   **Basic Error Handling:** Error handling is minimal.
*   **No Complex State Management:** Doesn't track conversation history, tools, etc.

## Prerequisites

*   **Go:** Version 1.18 or later recommended.
*   **A WAV Audio File:** You need a `.wav` file containing the audio you want the mock to stream back.
    *   **Format:** Ideally, this should be **16-bit PCM, single-channel (mono), 24kHz sample rate**, as this matches common formats expected by the Realtime API clients. The mock *attempts* to skip a 44-byte header but doesn't validate the format strictly.
*   **WebSocket Client:** A tool like Postman or a custom client application to connect to the mock server.

## Configuration (`config.yaml`)

Create a file named `config.yaml` in the same directory as the executable.

```yaml
# config.yaml - Simplified
server:
  port: 8080        # Port to listen on

mock:
  # Delay in seconds after receiving the *first* audio chunk before responding
  responseDelaySeconds: 5
  # Path to the WAV file to play back (e.g., ./my_audio.wav)
  audioWavPath: "./mock_audio.wav"
  # The complete mock transcript text to send back chunk by chunk
  transcriptText: "This is the simplified mock transcript being sent back."
  # How often to send audio/transcript chunks (milliseconds)
  chunkIntervalMs: 100
  # How many audio data bytes (after header) per chunk to send in response.audio.delta
  audioChunkSizeBytes: 4096
```

*   `server.port`: Port for the mock server.
*   `mock.responseDelaySeconds`: How long to wait (in seconds) after receiving the *first* audio input before starting the response stream.
*   `mock.audioWavPath`: Path to the `.wav` file that will be streamed back.
*   `mock.transcriptText`: The text that will be streamed back as the transcript.
*   `mock.chunkIntervalMs`: The delay between sending each chunk of audio and transcript data. Controls the streaming speed.
*   `mock.audioChunkSizeBytes`: The size (in bytes) of each audio data chunk read from the WAV file (after skipping the header) and sent in `response.audio.delta` events.

## Setup and Running

1.  **Save the Code:** Save the Go script provided previously as `main.go`.
2.  **Create `config.yaml`:** Create the configuration file as described above.
3.  **Prepare Audio File:** Place your desired `.wav` file (e.g., `mock_audio.wav`) in the same directory, ensuring its path matches the `audioWavPath` in `config.yaml`.
4.  **Get Dependencies:** Open a terminal in the directory and run:
    ```bash
    go mod init simplemock # Or your preferred module name
    go mod tidy
    ```
5.  **Build:**
    ```bash
    go build -o simple-mock-server .
    ```
6.  **Run:**
    ```bash
    ./simple-mock-server -config config.yaml
    # Or just ./simple-mock-server if config.yaml is the default name/location
    ```
    The server will start logging to the console.

## Testing with Postman

1.  Ensure the mock server is running.
2.  Open Postman and create a new **WebSocket Request**.
3.  Enter the Server URL: `ws://localhost:8080/v1/realtime` (adjust host/port if changed in config).
4.  Click **Connect**.
5.  In the "Messages" panel, you should see the `session.created` and `conversation.created` events arrive from the server.
6.  Go to the message composer section below the messages panel. Select `Text` (or `JSON`) format.
7.  Paste the following JSON message to simulate sending audio data:
    ```json
    {
      "type": "input_audio_buffer.append",
      "audio": "AAA=" // Minimal valid Base64 placeholder - content doesn't matter to mock
    }
    ```
8.  Click **Send**.
9.  Wait for the duration specified by `responseDelaySeconds` in your `config.yaml`.
10. Observe the "Messages" panel again. You should start seeing a stream of events from the server, including `response.created`, `response.audio.delta`, `response.text.delta`, and finally `response.done`.


## Docker examples
### Build the Docker Image
```bash
docker build -t simple-mock-server .
```
### Run the Docker Container
```bash
docker run -d -p 8080:8080 -v $(pwd)/config.yaml:/app/config.yaml simple-mock-server
```