// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config 鏄?sclient 鐨勯厤缃枃浠剁粨鏋勩€?
type Config struct {
	ServerURL    string `yaml:"server_url" mapstructure:"server_url"`
	Timeout      int    `yaml:"timeout" mapstructure:"timeout"`
	TunnelKey    string `yaml:"tunnel_key" mapstructure:"tunnel_key"`
	ChunkSize    int64  `yaml:"chunk_size" mapstructure:"chunk_size"`
	MaxChunkSize int64  `yaml:"max_chunk_size" mapstructure:"max_chunk_size"`
}

func DefaultConfig() *Config {
	return &Config{
		ServerURL: "http://localhost:18083",
		Timeout:   300,
		ChunkSize: size.DefaultChunkSize, // 4 MiB
	}
}

// Validate 鏍￠獙閰嶇疆鍚堢悊鎬э紝璁剧疆闆跺€煎瓧娈典负榛樿鍊笺€?
func (c *Config) Validate() error {
	if c.ServerURL == "" {
		c.ServerURL = "http://localhost:18083"
	}
	if c.Timeout <= 0 {
		c.Timeout = 300
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	if c.TunnelKey != "" && len(c.TunnelKey) != 64 {
		return fmt.Errorf("tunnel_key 蹇呴』鏄?64 浣?hex 瀛楃")
	}
	return nil
}

// LoadFromViper 浠?viper 瀹炰緥瑙ｇ爜閰嶇疆锛屽悎骞堕粯璁ゅ€煎苟鏍￠獙銆?
func LoadFromViper(v *viper.Viper) (*Config, error) {
	cfg := DefaultConfig()
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("閰嶇疆瑙ｇ爜澶辫触: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := SaveConfig(cfg, path); saveErr != nil {
				return nil, fmt.Errorf("鍒涘缓榛樿閰嶇疆鏂囦欢澶辫触: %w", saveErr)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("璇诲彇閰嶇疆鏂囦欢澶辫触: %w", err)
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("瑙ｆ瀽閰嶇疆鏂囦欢澶辫触: %w", err)
	}
	return cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("搴忓垪鍖栭厤缃け璐? %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("鍐欏叆閰嶇疆鏂囦欢澶辫触: %w", err)
	}
	return nil
}

func HandleConfigShow(cfg *Config) {
	fmt.Printf("ServerURL:     %s\n", cfg.ServerURL)
	fmt.Printf("Timeout:       %d\n", cfg.Timeout)
	maskedKey := cfg.TunnelKey
	if len(maskedKey) > 8 {
		maskedKey = maskedKey[:4] + "****" + maskedKey[len(maskedKey)-4:]
	}
	fmt.Printf("TunnelKey:     %s\n", maskedKey)
	fmt.Printf("ChunkSize:     %d\n", cfg.ChunkSize)
	fmt.Printf("MaxChunkSize:  %d\n", cfg.MaxChunkSize)
}

func HandleConfigSet(cfg *Config, configPath, key, value string) error {
	switch key {
	case "server_url":
		cfg.ServerURL = value
	case "timeout":
		if _, err := fmt.Sscanf(value, "%d", &cfg.Timeout); err != nil {
			return fmt.Errorf("鏃犳晥鐨勮秴鏃跺€? %w", err)
		}
	case "tunnel_key":
		cfg.TunnelKey = value
	case "chunk_size":
		if _, err := fmt.Sscanf(value, "%d", &cfg.ChunkSize); err != nil {
			return fmt.Errorf("鏃犳晥鐨勫垎鍧楀ぇ灏? %w", err)
		}
	case "max_chunk_size":
		if _, err := fmt.Sscanf(value, "%d", &cfg.MaxChunkSize); err != nil {
			return fmt.Errorf("鏃犳晥鐨勬渶澶у垎鍧楀ぇ灏? %w", err)
		}
	default:
		return fmt.Errorf("鏈煡閰嶇疆閿? %s", key)
	}
	return SaveConfig(cfg, configPath)
}
