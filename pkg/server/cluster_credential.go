// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cluster_credential.go 是集群出口凭证在目标节点侧的授权（2026-10-05 用户裁定）：
// 持有节点签发自包含短效凭证（HMAC+时效+scope+范围），目标节点 remote_read 授权层
// 在静态 mesh_readers 未命中时按对端指纹验签——**下发端控制时效/白名单/范围**。
//
// 装配：持有节点配置 `cluster.credentials`（签发方 SK + 凭证池），启动期解析 →
// clusterCredentialSet 注入 remote_read listener（每连接授权判定用）。

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
)

// clusterCredentialSet 是目标节点持有的集群出口凭证池（装配期解析，运行期只读）。
// clusterCredentialSet 是目标节点持有的集群出口凭证池（装配期解析，运行期只读）。
type clusterCredentialSet struct {
	// signKey 是签发方 SK（验签本节点签发的全部凭证；32B）。
	signKey []byte
	// creds 是凭证池：**按对端 xfer 指纹索引**（与 credentialFor 的查询键一致——
	// 评审 C1 修复：此前按 c.Node 写入导致查恒 miss，凭证授权恒死代码）。
	creds map[string]clustercred.Credential // 对端指纹（凭证.Recipient）→ 凭证
	log   *slog.Logger
}

// credentialFor 按对端指纹返回有效凭证（验签 + 时效 + scope + 卷范围）。
// 返回 (凭证, true) 表示该对端已被持有节点授权（替代静态 mesh_readers）。
//
// **volName 早拒（评审 C2 纵深）**：凭证限定的 Volume 与请求卷不符时直接返回
// (zero, false)——授权分支的 Authorizes 还会再校验（volume/path_prefix 双保险）。
func (s *clusterCredentialSet) credentialFor(volName, fingerprint string) (clustercred.Credential, bool) {
	if s == nil {
		return clustercred.Credential{}, false
	}
	cred, ok := s.creds[strings.TrimSpace(fingerprint)]
	if !ok {
		return clustercred.Credential{}, false
	}
	if volName != "" && cred.Volume != volName {
		// 凭证授的卷 != 请求卷 → 越权面（fail-closed，不泄凭证存在性）。
		return clustercred.Credential{}, false
	}
	if verr := cred.Verify(s.signKey, time.Now()); verr != nil {
		// 凭证过期/签名不符 → 视为未授权（fail-closed，不泄露原因）。
		if s.log != nil {
			s.log.Debug("集群出口凭证验签失败", "fingerprint", fingerprint, "err", verr)
		}
		return clustercred.Credential{}, false
	}
	return cred, true
}

// newClusterCredentialSet 解析配置的集群出口凭证池（cluster.credentials 段）。
// signKey 必填（32B）；凭证池空 → nil（未装配，零回归）。解析失败 → 返回错误（装配
// fail-closed——下发凭证语义不可静默丢弃）。
func newClusterCredentialSet(cfg *Config, log *slog.Logger) (*clusterCredentialSet, error) {
	if len(cfg.Cluster.Credentials) == 0 {
		// 未配置凭证池：无集群出口授权（静态 mesh_readers 照常，零回归）——
		// 即使 credential_sign_key 未配也不报错（只有配置了凭证才需要密钥）。
		return nil, nil
	}
	// 签发方 SK：配置了凭证池则必填（32B hex→字节）。
	skHex := cfg.Cluster.CredentialSignKey
	if len(skHex) != 64 {
		return nil, errClusterCredentialSignKey
	}
	sk := make([]byte, 32)
	if _, derr := hex.Decode(sk, []byte(skHex)); derr != nil {
		return nil, errClusterCredentialSignKey
	}
	creds := make(map[string]clustercred.Credential, len(cfg.Cluster.Credentials))
	for _, ec := range cfg.Cluster.Credentials {
		c, perr := clustercred.ParseCredential(ec.Encoded)
		if perr != nil {
			return nil, perr
		}
		// 验签（装配期即发现伪造/篡改凭证，fail-fast）。
		if verr := c.Verify(sk, time.Now()); verr != nil {
			return nil, verr
		}
		if c.Node == "" || c.Owner == "" || c.Recipient == "" {
			return nil, errClusterCredentialMalformed
		}
		// 按 Recipient（对端 xfer 指纹）索引——与 credentialFor 查询键一致
		// （评审 C1 修复：此前按 c.Node 索引，查恒 miss，凭证授权恒死代码）。
		// 同 Recipient 多条凭证 → 装配期冲突拒绝（fail-closed，不静默覆盖）。
		if _, dup := creds[c.Recipient]; dup {
			return nil, errClusterCredentialDup
		}
		creds[c.Recipient] = c
	}
	if len(creds) == 0 {
		return nil, errClusterCredentialEmpty
	}
	return &clusterCredentialSet{signKey: sk, creds: creds, log: log}, nil
}

// 装配哨兵错误（cluster.credentials 配置非法时 fail-closed）。
var (
	errClusterCredentialSignKey   = errors.New("cluster: 凭证签发密钥需 32B hex（cluster.credential_sign_key）")
	errClusterCredentialMalformed = errors.New("cluster: 凭证缺 node/owner/recipient（cluster.credentials）")
	errClusterCredentialDup       = errors.New("cluster: 凭证池中同一出口节点指纹重复（cluster.credentials）")
	errClusterCredentialEmpty     = errors.New("cluster: 凭证池为空（cluster.credentials）")
)
