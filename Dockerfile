# 1. Base Image
FROM golang:1.24-alpine AS builder

# 2. Working Directory
WORKDIR /app

# 3. Environment Variable
ENV GO111MODULE=on

# 4. Copy Dependency Files
COPY go.mod go.sum ./

# 5. Download Dependencies
RUN go mod download
RUN go mod verify

# 6. Copy Source Code (only .go files for build stage)
COPY *.go ./

# 7. Build Application
# Output the binary to /app/simple-mock-server in the builder stage
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/simple-mock-server .

# --- Release Stage ---
FROM alpine:latest

# Set working directory for release stage
WORKDIR /app

# Create a non-root user and group
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Copy the compiled application from the builder stage
COPY --from=builder /app/simple-mock-server /app/simple-mock-server

# 8. Copy Default Configuration
# Create a directory for the default configuration
RUN mkdir -p /app/config

# Copy config.yaml and mock_audio.wav into /app/config/
# These files must exist in the context root when 'docker build' is run.
COPY config.yaml /app/config/config.yaml
COPY mock_audio.wav /app/config/mock_audio.wav

# Copy static assets for the web UI
COPY static /app/static

# Ensure the /app/config and /app/static directories and their contents are owned by the appuser
RUN chown -R appuser:appgroup /app/config /app/static
# Ensure the server binary is executable and owned by appuser
RUN chown appuser:appgroup /app/simple-mock-server && chmod +x /app/simple-mock-server


# Switch to non-root user
USER appuser

# 9. Expose Port (as specified in default config.yaml or typically 8080)
EXPOSE 8080

# 10. Entrypoint/Command
# The application will be started with a command that specifies the config path.
# This assumes the Go application will be modified to accept a -config flag.
CMD ["/app/simple-mock-server", "-config", "/app/config/config.yaml"]
