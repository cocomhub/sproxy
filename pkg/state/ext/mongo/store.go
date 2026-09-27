// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mongo 是 StateStore / LeaderElector 的 MongoDB 实现（可插拔扩展独立 module，
// 仿 pkg/tunnel/xfer/ext/* 模式——依赖 go.mongodb.org/mongo-driver 隔离在独立 module，
// 领域包 pkg/state 核心不引）。
//
// 与设计 docs/designs/2026-09-24-statestore.md §2.5 / 2026-09-24-leader-elector.md
// §2.3 对齐：
//
//   - MongoStateStore：集合文档 `_id=key`，字段 `{v: <bytes>, rev: <int64>}`；
//     Put 用 findOneAndUpdate（upsert + $inc rev）原子写；CAS 用 rev 版本号 + 单文档
//     原子更新（filter 带 rev：读-改-写之间并发写导致 rev 不匹配 → ErrCASMismatch，
//     调用方按 409/重试处理——无读-改-写非原子序列）；
//   - MongoLeaderElector：`_id="leader"` 文档 `{holder, expires_at}` TTL 租约；
//     TryAcquire findOneAndUpdate upsert（$or: [holder=leaseID, expires_at<now]）；
//     Renew 未命中 → ErrLeaseLost（防旧主复活）；Release findOneAndDelete 幂等；
//     TTL index（expires_at + expireAfterSeconds:0）兜底清理崩溃残留。
//
// 安全边界：key 一律经 state.ValidateKey 校验（fail-closed，绝不静默改写——与
// LocalStateStore 同语义，同一 key 集两实现接受/拒绝一致）。
package mongo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/cocomhub/sproxy/pkg/state"
)

// stateDoc 是 StateStore 的 mongo 文档形态（设计 §2.5：{v, rev}）。
// _id = key（文档主键）；rev 单调递增（首次 = 1），诊断/观测用途。
type stateDoc struct {
	V   []byte `bson:"v"`
	Rev int64  `bson:"rev"`
}

// MongoStateStore 是 StateStore 的 MongoDB 实现（集群模式状态后端）。
//
// 语义对齐（与 LocalStateStore 逐条一致）：
//   - Get：未命中 ErrKeyNotFound；绝不返回 (nil, nil)；
//   - Put：原子 upsert（rev 自增），成功返回后读必见新值（mongo 主节点线性一致）；
//   - Delete：幂等（不存在静默成功）；
//   - List：prefix 前缀范围查询（_id ∈ [prefix, prefix+\uffff)）；
//   - CAS：old=nil 期望不存在 / new=nil 期望删除；失败 ErrCASMismatch。
type MongoStateStore struct {
	col    *mongo.Collection
	client *mongo.Client
	logger *slog.Logger
}

var _ state.StateStore = (*MongoStateStore)(nil)

// NewMongoStateStore 连接 mongo 并构造状态存储（构造即探活 Ping，失败返回错误——
// 装配期 fail-closed，不回落 local，防「以为多节点一致、实际各写各的」）。
//
// uri 为空 → 参数校验错误；logger nil → slog.Default()。
func NewMongoStateStore(uri, database, collection string, logger *slog.Logger) (*MongoStateStore, error) {
	if uri == "" {
		return nil, fmt.Errorf("mongo: uri 必填")
	}
	if database == "" {
		database = "sproxy"
	}
	if collection == "" {
		collection = "sproxy_state"
	}
	if logger == nil {
		logger = slog.Default()
	}
	client, err := connect(uri, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &MongoStateStore{
		col:    client.Database(database).Collection(collection),
		client: client,
		logger: logger,
	}, nil
}

// connect 建立 mongo 连接并 Ping 探活（超时 timeout；失败 Disconnect 后返回错误）。
func connect(uri string, timeout time.Duration) (*mongo.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo: 连接失败: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo: 探活失败（不可达，装配期 fail-closed）: %w", err)
	}
	return client, nil
}

// Close 释放 mongo 连接（幂等；装配层停服时调用）。
func (s *MongoStateStore) Close(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	err := s.client.Disconnect(ctx)
	s.client = nil
	return err
}

// Get 读取 key 的值；未命中返回 ErrKeyNotFound（绝不返回 (nil, nil)）。
func (s *MongoStateStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := state.ValidateKey(key); err != nil {
		return nil, err
	}
	var doc stateDoc
	err := s.col.FindOne(ctx, bson.M{"_id": key}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, state.ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mongo: Get(%s) 失败: %w", key, err)
	}
	return doc.V, nil
}

// Put 原子写 key 的值（findOneAndUpdate upsert + rev 自增）。
func (s *MongoStateStore) Put(ctx context.Context, key string, data []byte) error {
	if err := state.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.col.UpdateOne(ctx,
		bson.M{"_id": key},
		bson.M{"$set": bson.M{"v": data}, "$inc": bson.M{"rev": 1}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("mongo: Put(%s) 失败: %w", key, err)
	}
	return nil
}

// Delete 删除 key（幂等：不存在静默成功）。
func (s *MongoStateStore) Delete(ctx context.Context, key string) error {
	if err := state.ValidateKey(key); err != nil {
		return err
	}
	if _, err := s.col.DeleteOne(ctx, bson.M{"_id": key}); err != nil {
		return fmt.Errorf("mongo: Delete(%s) 失败: %w", key, err)
	}
	return nil
}

// List 返回 prefix 前缀下的完整 key 列表（排序不承诺稳定；调用方自行排序）。
// 范围查询 _id ∈ [prefix, prefix+"\uffff")——等价前缀匹配（Local 用目录 walk +
// HasPrefix，同一语义）。
func (s *MongoStateStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := state.ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	filter := bson.M{}
	if prefix != "" {
		filter = bson.M{"_id": bson.M{"$gte": prefix, "$lt": prefix + "\uffff"}}
	}
	cursor, err := s.col.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("mongo: List(%q) 失败: %w", prefix, err)
	}
	defer func() { _ = cursor.Close(ctx) }()
	var out []string
	for cursor.Next(ctx) {
		var doc struct {
			ID string `bson:"_id"`
		}
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mongo: List(%q) 解码失败: %w", prefix, err)
		}
		out = append(out, doc.ID)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("mongo: List(%q) 迭代失败: %w", prefix, err)
	}
	return out, nil
}

// CAS 原子比较并交换：当前值 == old 时替换为 new。
//
// 语义（对齐设计 §2.2）：old == nil 表示「期望不存在」（create-only）；new == nil
// 表示「期望删除」；失败返回 ErrCASMismatch。
// 实现：**单文档原子比对**（设计 §2.5「findAndModify 内嵌比对」等价语义）——
// filter 直接带 `v=old` 值比对（BSON Binary 精确匹配），不存在「读→判→写」的
// 非原子序列：
//   - old != nil（替换）：UpdateOne({_id, v: old}, {$set: v:new, $inc: rev:1})，
//     无 upsert。filter 不匹配（不存在/值变）→ MatchedCount=0 → ErrCASMismatch；
//     匹配 → ModifiedCount=1（$inc 保证即便 old==new 也产生修改）；
//   - old == nil（create-only）：UpdateOne({_id}, {$setOnInsert: {v:new, rev:1}},
//     upsert)。UpsertedCount=1 → 成功；MatchedCount=1（已存在）→ ErrCASMismatch
//     （$setOnInsert 在已存在时 no-op，不会覆盖现有值）；
//   - new == nil（删除）：DeleteOne({_id, v: old})。DeletedCount=1 → 成功；=0 时
//     区分「不存在」（old==nil 幂等成功 / old!=nil ErrCASMismatch）与「存在但值
//     不匹配」→ ErrCASMismatch。
// 并发写（读后、写前被改）→ filter 值不匹配 → ErrCASMismatch，调用方重试
// （无丢失更新——由并发 CAS 递增测试验收）。
func (s *MongoStateStore) CAS(ctx context.Context, key string, old, new []byte) error {
	if err := state.ValidateKey(key); err != nil {
		return err
	}
	// new == nil → 期望删除（单文档原子删；幂等：不存在 no-op）。
	if new == nil {
		res, derr := s.col.DeleteOne(ctx, bson.M{"_id": key, "v": old})
		if derr != nil {
			return fmt.Errorf("mongo: CAS(%s) 删除失败: %w", key, derr)
		}
		if res.DeletedCount == 0 {
			cnt, cerr := s.col.CountDocuments(ctx, bson.M{"_id": key})
			if cerr != nil {
				return fmt.Errorf("mongo: CAS(%s) 存在性检查失败: %w", key, cerr)
			}
			if cnt == 0 {
				if old == nil {
					return nil // 幂等删除（期望不存在且确实不存在）
				}
				return state.ErrCASMismatch // 期望存在但缺失
			}
			return state.ErrCASMismatch // 存在但值不匹配
		}
		return nil
	}
	// old == nil（create-only）：$setOnInsert + upsert——不存在原子创建；已存在
	// no-op（MatchedCount=1，不覆盖现有值）→ ErrCASMismatch。
	if old == nil {
		res, uerr := s.col.UpdateOne(ctx,
			bson.M{"_id": key},
			bson.M{"$setOnInsert": bson.M{"v": new, "rev": 1}},
			options.Update().SetUpsert(true),
		)
		if uerr != nil {
			return fmt.Errorf("mongo: CAS(%s) create-only 失败: %w", key, uerr)
		}
		if res.UpsertedCount == 1 {
			return nil
		}
		return state.ErrCASMismatch // MatchedCount=1：已存在
	}
	// 常规替换（old != nil）：filter 带 v 值比对，无 upsert（防并发删除后凭空重建
	// ——期望存在则必须匹配存在）。$inc 保证即便 old==new 也产生修改（ModifiedCount=1）。
	res, uerr := s.col.UpdateOne(ctx,
		bson.M{"_id": key, "v": old},
		bson.M{"$set": bson.M{"v": new}, "$inc": bson.M{"rev": 1}},
	)
	if uerr != nil {
		return fmt.Errorf("mongo: CAS(%s) 更新失败: %w", key, uerr)
	}
	if res.MatchedCount == 0 {
		return state.ErrCASMismatch // 不存在或值不匹配
	}
	return nil
}
