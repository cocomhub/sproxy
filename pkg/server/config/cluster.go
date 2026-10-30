// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cluster.go：集群与状态存储配置段的数据模型与校验。
package config

import (
	"errors"
	"fmt"
	"time"
)

// StateStoreConfig 是状态存储后端配置（state_store 段，statestore.md §5.3）。
// Type 为 local（缺省，零回归）| mongo | raft（raft 未实现，装配报错 fail-closed）；
// Dir 为 local 专属状态根（空 = <storage_root>/state）；Mongo 为 mongo 后端连接配置
// （F3 片消费，本期装配传递）。
type StateStoreConfig struct {
	Type  string      `yaml:"type" mapstructure:"type"`
	Dir   string      `yaml:"dir" mapstructure:"dir"`
	Mongo MongoConfig `yaml:"mongo" mapstructure:"mongo"`
}

// Validate 校验 state_store 段：type 空/local 合法；raft 响亮拒绝（未实现，fail-closed）。
// mongo 必填 uri（F3 装配探活）；未启用（type 空 = 单节点零回归）时不校验 mongo。
func (c StateStoreConfig) Validate() error {
	switch c.Type {
	case "", "local":
		return nil
	case "raft":
		return errors.New("state_store.type=raft 未实现（etcd/raft 复制状态机，roadmap 11.12-F5）")
	case "mongo":
		if c.Mongo.URI == "" {
			return errors.New("state_store.type=mongo 需配置 state_store.mongo.uri")
		}
		return nil
	default:
		return fmt.Errorf("state_store.type=%q 无效，仅支持 local|mongo|raft（raft 未实现）", c.Type)
	}
}

// MongoConfig 是 mongo 后端子配置（state_store.mongo.*；F3 MongoStateStore 消费）。
type MongoConfig struct {
	URI        string `yaml:"uri" mapstructure:"uri"`
	Database   string `yaml:"database" mapstructure:"database"`
	Collection string `yaml:"collection" mapstructure:"collection"`
}

// ClusterRole 是节点角色。
const (
	ClusterRoleMaster  = "master"
	ClusterRoleReplica = "replica"
)

// ClusterConfig 是集群节点配置（cluster 段）。空 = 单节点（零回归）。
type ClusterConfig struct {
	Enabled        bool          `yaml:"enabled" mapstructure:"enabled"`
	NodeID         string        `yaml:"node_id" mapstructure:"node_id"`                             // 必填（Enabled 时）；跨节点唯一
	Role           string        `yaml:"role" mapstructure:"role"`                                   // master | replica；默认 master
	ResyncInterval time.Duration `yaml:"index_resync_interval" mapstructure:"index_resync_interval"` // 索引 resync 兜底周期（默认 5m；0 = 关闭）

	// CredentialSignKey 是集群出口凭证签发方 SK（32B，2026-10-05 用户裁定凭证下发）：
	// 持有节点用它签发自包含短效凭证；目标节点侧持同 key 验签授权出口节点。
	CredentialSignKey string `yaml:"credential_sign_key,omitempty" mapstructure:"credential_sign_key"`
	// Credentials 是集群出口凭证池（目标节点侧装配）：多条凭证，每条指向不同出口节点
	// 指纹（签发后经配置下发到目标节点）。
	Credentials []ClusterCredentialConfig `yaml:"credentials,omitempty" mapstructure:"credentials"`
}

// Validate 校验集群段（未启用 → 恒通过零回归）。
func (c ClusterConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.NodeID == "" {
		return errors.New("cluster: enabled 时 node_id 必填")
	}
	if c.Role != ClusterRoleMaster && c.Role != ClusterRoleReplica {
		return fmt.Errorf("cluster: role 必须为 %q 或 %q（got %q）", ClusterRoleMaster, ClusterRoleReplica, c.Role)
	}
	if c.ResyncInterval < 0 {
		return errors.New("cluster: index_resync_interval 不能为负")
	}
	return nil
}

// ClusterCredentialConfig 是一条集群出口凭证的配置载体（目标节点装配用）。
// Encoded 是签发方 Marshal 的 Credential 串（含 Sig；目标节点验签授权）。
type ClusterCredentialConfig struct {
	Encoded string `yaml:"encoded" mapstructure:"encoded"`
}
