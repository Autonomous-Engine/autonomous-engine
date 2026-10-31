package config

import (
	"encoding/json"
	"os"
)

// Config is the service configuration.
type Config struct {
	Port int `json:"port"`
}

// Load reads the configuration file at path.
func Load(path string) (Config, error) {
	var c Config
	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		return Config{Port: 8080}, nil
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}
