// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// 功能桶白名单：租户根下物理隔离的目录名。
const (
	bucketUser    = "user"
	bucketCloud   = "cloud"
	bucketArchive = "archive"
	bucketChunk   = "chunk"
	bucketVersion = "version"
	bucketMeta    = "meta"
)

// featureBuckets 是 FeatureRel 允许的桶白名单。
var featureBuckets = []string{bucketUser, bucketCloud, bucketArchive, bucketChunk, bucketVersion, bucketMeta}

// Tenant 持有自己的 *Root 与目录布局。不持有配额类型（避免 pkg/storage → pkg/quota 依赖；
// 配额由 pkg/server 用 map[string]*quota.Scope 按 tenant.ID 关联）。
type Tenant struct {
	ID   string // 合法段名
	root *Root
}

// NewTenant 构造租户值类型。owner 必须通过 ValidSegmentName（fail-closed），
// 非法 owner 返回错误（绝不回落全局根）。root 不能为 nil。
func NewTenant(owner string, root *Root) (*Tenant, error) {
	if root == nil {
		return nil, errors.New("storage: NewTenant: root 不能为 nil")
	}
	if !ValidSegmentName(owner) {
		return nil, fmt.Errorf("storage: NewTenant: 非法租户名 %q", owner)
	}
	return &Tenant{ID: owner, root: root}, nil
}

// Root 返回租户持有的存储根。
func (t *Tenant) Root() *Root { return t.root }

// UserRoot 返回用户文件桶名（"user"）。用户文件根 = Tenant.Root() 下的该目录。
func (t *Tenant) UserRoot() string { return bucketUser }

// Buckets 返回租户根下全部功能桶名（顺序稳定）。
func (t *Tenant) Buckets() []string {
	return []string{bucketUser, bucketCloud, bucketArchive, bucketChunk, bucketVersion, bucketMeta}
}

// UserRel 将用户输入路径归一并映射到 user 桶下的安全相对路径。
// 内部先 NormalizeRemote（TrimSpace、拒绝空/绝对路径/.. 段、ToSlash），再逐段 ValidSegmentName
// 校验；首段为功能桶名或 __ 遗留前缀时拒绝。通过则返回 "user/<normalized>"。
func (t *Tenant) UserRel(remotePath string) (string, bool) {
	normalized, ok := NormalizeRemote(remotePath)
	if !ok {
		return "", false
	}
	segs := strings.Split(normalized, "/")
	for _, seg := range segs {
		if !ValidSegmentName(seg) {
			return "", false
		}
	}
	if isReservedUserFirstSegment(segs[0]) {
		return "", false
	}
	return bucketUser + "/" + normalized, true
}

// isReservedUserFirstSegment 判断用户输入首段是否为保留名：功能桶名或 __ 遗留内部前缀。
func isReservedUserFirstSegment(seg string) bool {
	switch seg {
	case bucketUser, bucketCloud, bucketArchive, bucketChunk, bucketVersion, bucketMeta:
		return true
	}
	return strings.HasPrefix(seg, "__")
}

// FeatureRel 构造服务端内部功能路径：bucket + "/" + sub。
// bucket 必须在白名单 [user cloud archive chunk version meta]；sub 经 NormalizeRemote（可为空，
// 为空时返回裸桶名）。未知 bucket 或非法 sub 返回 ok=false。
func (t *Tenant) FeatureRel(bucket, sub string) (string, bool) {
	if !isValidBucket(bucket) {
		return "", false
	}
	norm := strings.TrimSpace(sub)
	if norm == "" {
		return bucket, true
	}
	normalized, ok := NormalizeRemote(norm)
	if !ok {
		return "", false
	}
	return bucket + "/" + normalized, true
}

// isValidBucket 判断 bucket 是否在功能桶白名单内。
func isValidBucket(bucket string) bool {
	return slices.Contains(featureBuckets, bucket)
}
