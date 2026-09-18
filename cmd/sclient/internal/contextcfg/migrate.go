// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package contextcfg

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// MigrateLegacy 读取旧平铺 sclient.yaml（client.Config 字段形态）并映射为新
// context 配置：连接面字段（server_url/hub_url/node_id/ca_file/insecure/timeout/
// chunk_size）→ Environments[0]，凭据面字段（access_key/access_key_secret/
// access_key_id）→ Users[0]，并生成一个指向两者的 context 且设为 current。
//
// envName 为空时环境名用 "default"（SCLIENT_ENV 未设置）；非空时用 envName
// （SCLIENT_ENV=prod → "prod"）。返回的新 Config 尚未落盘——由调用方决定保存
// 到 config.yaml。
//
// 非核心旧字段（MaxChunkSize/PeerFingerprints/AllowTransportFallback 等）不迁移
// （最小改动：迁移只保 context 三件套能表达的连接/凭据面；其余需时由用户补配）。
func MigrateLegacy(legacyPath, envName string) (*Config, error) {
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("旧配置文件 %s 为空", legacyPath)
	}
	// 用最小结构承接旧字段（不依赖 pkg/client.Config 以避免 contextcfg 引入
	// 对根 module 包的依赖——cmd/sclient 已依赖根 module，但保持包独立更干净）。
	var legacy struct {
		ServerURL       string `yaml:"server_url"`
		Timeout         int    `yaml:"timeout"`
		ChunkSize       int64  `yaml:"chunk_size"`
		AccessKey       string `yaml:"access_key"`
		AccessKeySecret string `yaml:"access_key_secret"`
		AccessKeyID     string `yaml:"access_key_id"`
		HubURL          string `yaml:"hub_url"`
		NodeID          string `yaml:"node_id"`
		XferCAFile      string `yaml:"xfer_ca_file"`
		XferInsecure    bool   `yaml:"xfer_insecure"`
		Volume          string `yaml:"volume"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("解析旧配置文件 %s 失败: %w", legacyPath, err)
	}

	name := envName
	if name == "" {
		name = "default"
	}
	env := &Environment{
		Name:      name,
		ServerURL: legacy.ServerURL,
		HubURL:    legacy.HubURL,
		NodeID:    legacy.NodeID,
		CAFile:    legacy.XferCAFile,
		Insecure:  legacy.XferInsecure,
		Timeout:   legacy.Timeout,
		ChunkSize: legacy.ChunkSize,
	}
	// Volume 挂到环境（context 覆盖字段之一；旧配置默认卷迁移过来）。
	ctx := &Context{
		Name:        name,
		Environment: name,
		User:        name,
		Volume:      legacy.Volume,
	}
	user := &User{
		Name:            name,
		AccessKey:       legacy.AccessKey,
		AccessKeySecret: legacy.AccessKeySecret,
		AccessKeyID:     legacy.AccessKeyID,
	}
	cfg := NewDefault()
	cfg.Environments = append(cfg.Environments, env)
	cfg.Users = append(cfg.Users, user)
	cfg.Contexts = append(cfg.Contexts, ctx)
	cfg.CurrentContext = name
	return cfg, nil
}
