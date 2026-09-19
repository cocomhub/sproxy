// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package contextcfg 提供 sclient 多环境多用户 context 配置模型（kubectl 式
// environments/users/contexts 三件套 + current-context 指针），供 cmd/sclient
// 读取/解析/迁移。设计见 docs/superpowers/specs/2026-09-19-sclient-context-refactor-design.md。
package contextcfg

import (
	"fmt"
)

// APIVersion 是 context 配置文件的版本标识。
const APIVersion = "sclient/v1"

// Kind 是 context 配置文件的类型标识。
const Kind = "Config"

// TURNConfig 是 TURN 中继服务器配置（mesh/p2p 打洞对称 NAT 兜底）。
type TURNConfig struct {
	URI  string `yaml:"uri" json:"uri"`
	User string `yaml:"user,omitempty" json:"user,omitempty"`
	Pass string `yaml:"pass,omitempty" json:"pass,omitempty"`
}

// Environment 描述一个连接面（sproxy 服务端 + mesh/hub 面）。
type Environment struct {
	Name          string       `yaml:"name" json:"name"`
	ServerURL     string       `yaml:"server_url,omitempty" json:"server_url,omitempty"`
	HubURL        string       `yaml:"hub_url,omitempty" json:"hub_url,omitempty"`
	NodeID        string       `yaml:"node_id,omitempty" json:"node_id,omitempty"`
	CAFile        string       `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	Insecure      bool         `yaml:"insecure,omitempty" json:"insecure,omitempty"`
	TURN          []TURNConfig `yaml:"turn,omitempty" json:"turn,omitempty"`
	STUN          []string     `yaml:"stun,omitempty" json:"stun,omitempty"`
	VirtualSubnet string       `yaml:"virtual_subnet,omitempty" json:"virtual_subnet,omitempty"`
	// Timeout（秒）/ ChunkSize（字节）是旧平铺迁移携带的调优项（非 context 核心
	// 字段；其余旧字段——MaxChunkSize/PeerFingerprints/AllowTransportFallback——
	// 迁移不携带，需要时由用户在 context set / config set 补齐）。
	Timeout   int   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	ChunkSize int64 `yaml:"chunk_size,omitempty" json:"chunk_size,omitempty"`
}

// User 描述一个凭据面（SproxySig access_key 三件套 + owner 用户名）。
type User struct {
	Name            string `yaml:"name" json:"name"`
	AccessKey       string `yaml:"access_key,omitempty" json:"access_key,omitempty"`
	AccessKeySecret string `yaml:"access_key_secret,omitempty" json:"access_key_secret,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty" json:"access_key_id,omitempty"`
	Owner           string `yaml:"owner,omitempty" json:"owner,omitempty"`
}

// Context 是 environment + user 的组合（可含卷覆盖）。
type Context struct {
	Name        string `yaml:"name" json:"name"`
	Environment string `yaml:"environment" json:"environment"`
	User        string `yaml:"user" json:"user"`
	Volume      string `yaml:"volume,omitempty" json:"volume,omitempty"`
}

// Config 是 context 配置文件的根结构（新单一事实源 ~/.config/sproxy/config.yaml）。
type Config struct {
	APIVersion     string         `yaml:"apiVersion" json:"apiVersion"`
	Kind           string         `yaml:"kind" json:"kind"`
	CurrentContext string         `yaml:"current-context,omitempty" json:"current-context,omitempty"`
	Environments   []*Environment `yaml:"environments" json:"environments"`
	Users          []*User        `yaml:"users" json:"users"`
	Contexts       []*Context     `yaml:"contexts" json:"contexts"`
}

// NewDefault 返回带版本/类型标识的空模型（供新配置初始化）。
func NewDefault() *Config {
	return &Config{
		APIVersion:   APIVersion,
		Kind:         Kind,
		Environments: []*Environment{},
		Users:        []*User{},
		Contexts:     []*Context{},
	}
}

// FindEnvironment 按名查找 environment（不存在返回 nil）。
func (c *Config) FindEnvironment(name string) *Environment {
	for _, e := range c.Environments {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// FindUser 按名查找 user（不存在返回 nil）。
func (c *Config) FindUser(name string) *User {
	for _, u := range c.Users {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// FindContext 按名查找 context（不存在返回 nil）。
func (c *Config) FindContext(name string) *Context {
	for _, ctx := range c.Contexts {
		if ctx.Name == name {
			return ctx
		}
	}
	return nil
}

// Validate 校验配置自洽性：名字唯一、context 引用存在、current-context 存在。
// 返回 nil 表示合法；否则返回描述第一个问题的错误（含相关名字便于定位）。
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("配置为空")
	}
	// 版本/类型仅当非空时校验（内存构造可缺省，Save 会补全；Load 已补全后校验）。
	if c.APIVersion != "" && c.APIVersion != APIVersion {
		return fmt.Errorf("配置版本不匹配: apiVersion=%q（期望 %s）", c.APIVersion, APIVersion)
	}
	if c.Kind != "" && c.Kind != Kind {
		return fmt.Errorf("配置类型不匹配: kind=%q（期望 %s）", c.Kind, Kind)
	}
	seenEnv := map[string]bool{}
	for _, e := range c.Environments {
		if e == nil || e.Name == "" {
			return fmt.Errorf("environment 名不能为空")
		}
		if seenEnv[e.Name] {
			return fmt.Errorf("environment 名重复: %q", e.Name)
		}
		seenEnv[e.Name] = true
	}
	seenUser := map[string]bool{}
	for _, u := range c.Users {
		if u == nil || u.Name == "" {
			return fmt.Errorf("user 名不能为空")
		}
		if seenUser[u.Name] {
			return fmt.Errorf("user 名重复: %q", u.Name)
		}
		seenUser[u.Name] = true
	}
	seenCtx := map[string]bool{}
	for _, ctx := range c.Contexts {
		if ctx == nil || ctx.Name == "" {
			return fmt.Errorf("context 名不能为空")
		}
		if seenCtx[ctx.Name] {
			return fmt.Errorf("context 名重复: %q", ctx.Name)
		}
		seenCtx[ctx.Name] = true
		if !seenEnv[ctx.Environment] {
			return fmt.Errorf("context %q 引用的 environment %q 不存在", ctx.Name, ctx.Environment)
		}
		if !seenUser[ctx.User] {
			return fmt.Errorf("context %q 引用的 user %q 不存在", ctx.Name, ctx.User)
		}
	}
	if c.CurrentContext != "" && !seenCtx[c.CurrentContext] {
		return fmt.Errorf("current-context %q 不存在", c.CurrentContext)
	}
	return nil
}

// Normalize 归一化名字列表显示（helpers 用，避免裸拼接）。
