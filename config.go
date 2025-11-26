package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"encoding/binary"

	"gopkg.in/yaml.v3"
)

// --- Configuration Structs ---

type ServerConfig struct {
	Port int `yaml:"port"`
}

type MockConfig struct {
	ResponseDelaySeconds int    `yaml:"responseDelaySeconds"`
	AudioWavPath         string `yaml:"audioWavPath"`
	ChunkIntervalMs      int    `yaml:"chunkIntervalMs"`
	AudioChunkSizeBytes  int    `yaml:"audioChunkSizeBytes"`
}

type ProxyConfig struct {
	URL           string `yaml:"url"`
	RecordingPath string `yaml:"recordingPath"`
	Model         string `yaml:"model"`
}

type Event struct {
	Type         string                  `yaml:"type"` // "message", "function_call", "user_transcription"
	DelayMs      int                     `yaml:"delay_ms"`
	Text         string                  `yaml:"text,omitempty"`          // For "message" and "user_transcription"
	FunctionCall *FunctionCallDefinition `yaml:"function_call,omitempty"` // For "function_call"
}

type FunctionCallDefinition struct {
	Name      string `yaml:"name"`
	Arguments string `yaml:"arguments"` // JSON string of arguments
}

type Scenario struct {
	Name   string  `yaml:"name"`
	Events []Event `yaml:"events"`
}

type Config struct {
	Server             ServerConfig `yaml:"server"`
	Mock               MockConfig   `yaml:"mock"`
	Proxy              ProxyConfig  `yaml:"proxy"`
	Mode               string       `yaml:"mode"`
	LogInboundMessages bool         `yaml:"logInboundMessages"`
	Scenarios          []Scenario   `yaml:"scenarios"`
}

// --- Global Variables ---

var appConfig Config // Loaded config

const (
	customConfigPath = "/app/custom_config/config.yaml"
	// Default config path if -config flag is not provided or for Docker's CMD
	defaultConfigFlagValue = "config.yaml"
)

// loadConfiguration loads the application configuration.
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

	// Validate configuration
	if err := validateConfig(&appConfig); err != nil {
		return selectedConfigFile, fmt.Errorf("configuration validation failed: %w", err)
	}

	// Resolve audioWavPath
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

func validateConfig(cfg *Config) error {
	// Only validate scenarios if we are in mock mode, or just warn?
	// The original code validated scenarios always.
	if len(cfg.Scenarios) == 0 && cfg.Mode == "mock" {
		return fmt.Errorf("no scenarios defined in configuration for mock mode")
	}

	scenarioNames := make(map[string]bool)
	for _, scenario := range cfg.Scenarios {
		if scenario.Name == "" {
			return fmt.Errorf("scenario found with empty name")
		}
		if scenarioNames[scenario.Name] {
			return fmt.Errorf("duplicate scenario name: %s", scenario.Name)
		}
		scenarioNames[scenario.Name] = true

		for i, event := range scenario.Events {
			if event.Type != "message" && event.Type != "function_call" && event.Type != "user_transcription" {
				return fmt.Errorf("scenario '%s' event %d has unknown type: %s", scenario.Name, i, event.Type)
			}
			if event.Type == "function_call" && (event.FunctionCall == nil || event.FunctionCall.Name == "") {
				return fmt.Errorf("scenario '%s' event %d (function_call) missing function name", scenario.Name, i)
			}
		}
	}
	return nil
}

// validateWavFormat checks if the WAV file is 24kHz PCM16 Mono
func validateWavFormat(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read header (first 44 bytes)
	header := make([]byte, 44)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("failed to read WAV header: %w", err)
	}

	// Check RIFF and WAVE
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return fmt.Errorf("invalid WAV file format")
	}

	// Check AudioFormat (PCM = 1) - bytes 20-21
	audioFormat := binary.LittleEndian.Uint16(header[20:22])
	if audioFormat != 1 {
		return fmt.Errorf("audio format is not PCM (expected 1, got %d)", audioFormat)
	}

	// Check NumChannels (Mono = 1) - bytes 22-23
	numChannels := binary.LittleEndian.Uint16(header[22:24])
	if numChannels != 1 {
		return fmt.Errorf("audio is not mono (expected 1 channel, got %d)", numChannels)
	}

	// Check SampleRate (24000) - bytes 24-27
	sampleRate := binary.LittleEndian.Uint32(header[24:28])
	if sampleRate != 24000 {
		return fmt.Errorf("sample rate is not 24kHz (expected 24000, got %d)", sampleRate)
	}

	// Check BitsPerSample (16) - bytes 34-35
	bitsPerSample := binary.LittleEndian.Uint16(header[34:36])
	if bitsPerSample != 16 {
		return fmt.Errorf("bits per sample is not 16 (expected 16, got %d)", bitsPerSample)
	}

	return nil
}

func initConfig() {
	cliConfigPath := flag.String("config", defaultConfigFlagValue, "Path to the configuration file")
	flag.Parse()

	loadedConfigFile, err := loadConfiguration(*cliConfigPath)
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	log.Printf("Successfully loaded and processed configuration from %s", loadedConfigFile)

	if appConfig.Server.Port == 0 {
		appConfig.Server.Port = 8080
	}
	if appConfig.Mock.AudioChunkSizeBytes == 0 {
		appConfig.Mock.AudioChunkSizeBytes = 4096
	}
	if appConfig.Mock.ChunkIntervalMs == 0 {
		appConfig.Mock.ChunkIntervalMs = 100
	}

	// Check if audio file exists and validate format (after path resolution)
	if appConfig.Mock.AudioWavPath != "" { // Only check if a path is configured
		if _, err := os.Stat(appConfig.Mock.AudioWavPath); os.IsNotExist(err) {
			log.Printf("WARNING: Audio file specified in config does not exist: %s", appConfig.Mock.AudioWavPath)
			log.Printf("WARNING: Audio playback will fail if this path is used.")
		} else {
			log.Printf("Audio file found: %s", appConfig.Mock.AudioWavPath)
			if err := validateWavFormat(appConfig.Mock.AudioWavPath); err != nil {
				log.Printf("WARNING: Audio file validation failed: %v", err)
			} else {
				log.Printf("Audio file format validated: 24kHz PCM16")
			}
		}
	} else {
		log.Printf("WARNING: No audioWavPath configured. Audio playback will not occur.")
	}
}
