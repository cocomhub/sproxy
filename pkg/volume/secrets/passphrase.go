// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"golang.org/x/crypto/scrypt"
)

// 双口令派生（可记忆、可重建的 opt-in 补充；设计
// docs/designs/2026-10-03-dual-passphrase-secret.md）：
//
//	secret = scrypt( SHA256(口令A), SHA256(口令B) 作 salt, high 档 2^17, 32B )
//
//   - 双独立秘密输入：口令B 的 SHA256 是秘密盐（第二因子），不是公开盐——破解需
//     同时猜中两个口令（组合熵 ~110-160bit，取决于口令强度；非 2^256）。
//   - 每口令独立 SHA-256 后级联作 salt：避免口令间相互推导，各口令熵独立叠加。
//   - KDF 用 scrypt（内存硬），不是 AES；复用 shardseal 已注册的 high 档参数
//     （AlgoByVersion(AlgoV1GCMHigh) 单一事实源——shardseal 提档位此处自动跟随）。
//   - 输出 32B→hex 64 字符，与随机 Create 产物同构，可写入 secrets 卷（0600）。
//
// 无任何需要存储的盐/密钥：重建 = 重输两口令 + 固定 high 档即得同一 secret
// （可恢复性）。最低下落：任何称「单/双口令能到 2^256」者不成立——真正 2^256 只能
// 靠随机成分（crypto/rand / 硬件 token）；口令模式熵上限由口令强度决定。

// deriver 持 scrypt 派生参数。**零值回落官方 high 档**（与公开 DerivePassphraseSecret
// 一致，防止测试/内部误用未知档位）；测试经字段注入低档规避 2^17 耗时（同一
// deriveSecret 逻辑、非 mock），档位变化 = 派生结果变化（类似 shardseal cross-tier
// fail-closed 语义）。
type deriver struct {
	n, r, p int
}

// effectiveN 返回有效 scrypt N：显式非零优先，零值回落官方 high 档（shardseal
// AlgoV1GCMHigh；单一事实源，不复制常量）。
func (d deriver) effectiveN() int {
	if d.n > 0 {
		return d.n
	}
	alg, ok := shardseal.AlgoByVersion(shardseal.AlgoV1GCMHigh)
	if !ok {
		// 注册表 init() 必然注册 AlgoV1GCMHigh（见 shardseal/crypto.go:init）；理论不可达。
		return 1 << 17
	}
	return alg.ScryptN
}

// effectiveRPC 返回 scrypt r/p：显式指定优先，否则回落官方 high 档参数。
func (d deriver) effectiveRPC() (int, int) {
	if d.r > 0 || d.p > 0 {
		if d.r <= 0 {
			d.r = 8
		}
		if d.p <= 0 {
			d.p = 1
		}
		return d.r, d.p
	}
	alg, ok := shardseal.AlgoByVersion(shardseal.AlgoV1GCMHigh)
	if !ok {
		return 8, 1
	}
	return alg.ScryptR, alg.ScryptP
}

// deriveSecret 是双口令派生的唯一实现：SHA256(口令A) 作密码、SHA256(口令B) 作盐，
// scrypt 得 32B → hex 64 字符。d 参数档位（零值回落 high）。
//
// 口令先 TrimSpace（重建容错：用户重建时多打/少打前后空格，仍得同一 secret；代价是
// 前导/尾随空格不参与熵——口令设计应避免依赖它们）。空/全空白口令 fail-closed
// （禁止低熵空口令派生——派生结果虽非空但熵来自口令，空口令将崩塌为空熵密钥）。
func deriveSecret(passA, passB string, d deriver) ([]byte, error) {
	a := strings.TrimSpace(passA)
	b := strings.TrimSpace(passB)
	if a == "" || b == "" {
		return nil, fmt.Errorf("secrets: 双口令派生口令不能为空（A=%q B=%q）", trimName(a), trimName(b))
	}
	hA := sha256.Sum256([]byte(a))
	hB := sha256.Sum256([]byte(b))
	n := d.effectiveN()
	r, p := d.effectiveRPC()
	key, err := scrypt.Key(hA[:], hB[:], n, r, p, 32)
	if err != nil {
		return nil, fmt.Errorf("secrets: 双口令 scrypt 派生失败: %w", err)
	}
	return []byte(hex.EncodeToString(key)), nil
}

// DerivePassphraseSecret 由两口令派生 32B hex secret（默认 high 档），**不落盘**。
// 重建同一 secret = 重输两口令 + 固定 high 档（可恢复性；无需要存储的盐/密钥）。
// 安全边界：熵由口令强度主导（组合 ~110-160bit），非 2^256（见包头与设计文档）。
func DerivePassphraseSecret(passA, passB string) ([]byte, error) {
	return deriveSecret(passA, passB, deriver{})
}

// CreateFromPassphrase 创建（或覆盖）一个**双口令派生** secret：默认 high 档派生
// 32B hex secret 写入 `secrets/<name>`（本地 0600）。与 Create 同构（同格式、同落盘），
// 但来源不同——此模式可记忆可重建，不依赖随机秘密。档位固定 high（不暴露，防误降）。
func (m *Manager) CreateFromPassphrase(ctx context.Context, name, passA, passB string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return nil, fmt.Errorf("%w %q", ErrInvalidSecretName, name)
	}
	key, err := DerivePassphraseSecret(passA, passB)
	if err != nil {
		return nil, err
	}
	if err := m.writeSecret(ctx, name, key); err != nil {
		return nil, err
	}
	return key, nil
}

// trimName 仅用于错误文案截断，避免整段口令入日志（口令属机密，日志只应见前缀）。
func trimName(s string) string {
	if len(s) < 3 {
		return s
	}
	return s[:3] + "…"
}
