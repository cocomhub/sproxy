// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ServerTimeouts struct {
	ReadHeader time.Duration `yaml:"read_header"`
	Read       time.Duration `yaml:"read"`
	Write      time.Duration `yaml:"write"`
	Idle       time.Duration `yaml:"idle"`
}

type Config struct {
	Addr           string         `yaml:"addr"`
	UploadsDir     string         `yaml:"uploads_dir"`
	AllowedHosts   []string       `yaml:"allowed_hosts"`
	ServerTimeouts ServerTimeouts `yaml:"server_timeouts"`
	ClientTimeout  time.Duration  `yaml:"client_timeout"`
	LogLevel       string         `yaml:"log_level"`
	LogFormat      string         `yaml:"log_format"`
	MaxHeaderBytes int            `yaml:"max_header_bytes"`
}

func Default() *Config {
	return &Config{
		Addr:       ":18080",
		UploadsDir: "./uploads",
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
