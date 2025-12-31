package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config holds application-wide settings.
type Config struct {
	PythonBinary string `json:"python_binary"`
	LogLevel     string `json:"log_level"`
}

var (
	// Global config instance (lazy-loaded).
	instance *Config
)

// LoadConfig reads and parses config.json from the project root.
// If the file doesn't exist, returns a default Config.
func LoadConfig(filePath string) (*Config, error) {
	if instance != nil {
		return instance, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return defaults if file doesn't exist.
			instance = &Config{
				PythonBinary: "/usr/bin/python3",
				LogLevel:     "info",
			}
			return instance, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	instance = &Config{}
	if err := json.Unmarshal(data, instance); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	return instance, nil
}

// Get returns the global config instance (assumes LoadConfig was called first).
func Get() *Config {
	if instance == nil {
		// Fallback to defaults if not loaded.
		return &Config{
			PythonBinary: "/usr/bin/python3",
			LogLevel:     "info",
		}
	}
	return instance
}
