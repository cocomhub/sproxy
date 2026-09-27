// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

// leaseDoc 是 LeaderElector 的 mongo 租约文档（设计 leader-elector.md §2.3）：
//   - holder：持有者 leaseID（如 node_id + 启动随机后缀）；
//   - expires_at：到期时间（持有者本地时钟写入；其它节点用自己时钟比对——
//     时钟偏移容忍由 ttl/3 续租周期消化，ttl 默认 30s、续租 10s、容忍 ±10s）；
//   - TTL index（expires_at + expireAfterSeconds:0）：进程崩溃后文档到期自动清理
//     （兜底；正常释放走 Release 主动删除）。
type leaseDoc struct {
	ID        string    `bson:"_id"`
	Holder    string    `bson:"holder"`
	ExpiresAt time.Time `bson:"expires_at"`
}

// MongoLeaderElector 是 LeaderElector 的 MongoDB TTL 租约实现（多节点共享存储场景
// 的真选主；Local flock 仅单机）。
//
// 语义（与 LocalLeaderElector 对齐）：
//   - TryAcquire：findOneAndUpdate upsert（$or: [holder=leaseID, 过期]）→ matched=1
//     成功 (true, nil)；被他人持有 (false, nil)（非错误）；ttl<=0 / leaseID 空 → 参数错误；
//   - Renew：filter holder=leaseID 更新 expires_at=now+ttl → 未命中 ErrLeaseLost
//     （防旧主复活：新主抢占后旧主 Renew 必失败 → 降级只读 fail-closed）；
//   - Release：findOneAndDelete {_id, holder=leaseID}（幂等：未持有/他人持有 no-op）。
type MongoLeaderElector struct {
	col    *mongo.Collection
	client *mongo.Client
	ttl    time.Duration // 构造时注入的租约时长（NewMongoLeaderElector 用；TryAcquire 的 ttl 参数优先）
	logger *slog.Logger
}

var _ state.LeaderElector = (*MongoLeaderElector)(nil)

// leaseID is the document id for the leader lease.
const leaseID = "leader"

// NewMongoLeaderElector 连接 mongo 并构造 TTL 租约选主。
//
// ttlHint 是构造级默认租约时长（TryAcquire 显式传 ttl 时以参数为准；0 → 默认 30s）。
// 构造即探活 Ping + 确保 TTL index（expires_at, expireAfterSeconds:0）存在——
// TTL index 是崩溃清理兜底，必须在装配期建好（fail-fast，不静默缺失）。
func NewMongoLeaderElector(uri, database, collection string, ttlHint time.Duration, logger *slog.Logger) (*MongoLeaderElector, error) {
	if uri == "" {
		return nil, fmt.Errorf("mongo: uri 必填")
	}
	if database == "" {
		database = "sproxy"
	}
	if collection == "" {
		collection = "sproxy_state"
	}
	if ttlHint <= 0 {
		ttlHint = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	client, err := connect(uri, 5*time.Second)
	if err != nil {
		return nil, err
	}
	col := client.Database(database).Collection(collection)
	// TTL index 兜底（expireAfterSeconds: 0——文档过期即删，崩溃残留自动清理）。
	idxCtx, idxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer idxCancel()
	if _, err := col.Indexes().CreateOne(idxCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	}); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo: 创建 TTL index 失败（租约兜底清理缺失，fail-fast）: %w", err)
	}
	return &MongoLeaderElector{
		col:    col,
		client: client,
		ttl:    ttlHint,
		logger: logger,
	}, nil
}

// Close 释放 mongo 连接（幂等）。
func (e *MongoLeaderElector) Close(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	err := e.client.Disconnect(ctx)
	e.client = nil
	return err
}

// TryAcquire 尝试获得租约。leaseID 空 / ttl <= 0 → 参数校验错误（fail-fast，与
// LocalLeaderElector 同语义——防「无限期持有」的隐式语义）。已被他人持有 →
// (false, nil)（非错误）。
//
// 原子性：单文档 findOneAndUpdate（upsert），filter 为
// `{_id:"leader", $or: [{holder: leaseID}, {expires_at: {$lt: now}}]}`——
// 不存在 / 已过期 / 本是自己的租约（续期）→ 原子接管。并发抢占时 mongo 保证
// 恰好一个 upsert 成功（matched=1），其余 (false, nil)。
func (e *MongoLeaderElector) TryAcquire(ctx context.Context, leaseID string, ttl time.Duration) (bool, error) {
	if leaseID == "" {
		return false, fmt.Errorf("mongo: leaseID 不能为空")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("mongo: ttl 必须 > 0（显式给租约时长）")
	}
	now := time.Now()
	update := bson.M{
		"$set": bson.M{
			"holder":     leaseID,
			"expires_at": now.Add(ttl),
		},
	}
	// ReturnDocument.After 返回更新后文档；upsert 使不存在时原子创建。
	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)
	res := e.col.FindOneAndUpdate(ctx,
		bson.M{
			"_id": "leader",
			"$or": []bson.M{
				{"holder": leaseID},
				{"expires_at": bson.M{"$lt": now}},
			},
		},
		update,
		opts,
	)
	if err := res.Err(); err != nil {
		// upsert 在 filter 不匹配（文档已存在且未过期且 holder 非本节点）时
		// 尝试插入新文档 → duplicate key（_id 已存在）——即「被他人持有」。
		// 这是正常失败路径（非错误），映射为 (false, nil)。
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, fmt.Errorf("mongo: TryAcquire 失败: %w", err)
	}
	var doc leaseDoc
	if err := res.Decode(&doc); err != nil {
		return false, fmt.Errorf("mongo: TryAcquire 解码失败: %w", err)
	}
	return true, nil
}

// Renew 续租本 leaseID 持有的租约。未持有 / 租约已过期被他人接管 → ErrLeaseLost
// （fail-closed：调用方立即降级只读，防旧主复活）。
func (e *MongoLeaderElector) Renew(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return fmt.Errorf("mongo: leaseID 不能为空")
	}
	res := e.col.FindOneAndUpdate(ctx,
		bson.M{"_id": "leader", "holder": leaseID},
		bson.M{"$set": bson.M{"expires_at": time.Now().Add(e.ttl)}},
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return state.ErrLeaseLost
		}
		return fmt.Errorf("mongo: Renew 失败: %w", err)
	}
	return nil
}

// Release 释放本 leaseID 持有的租约（幂等：未持有 / 他人持有 no-op 成功）。
func (e *MongoLeaderElector) Release(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return fmt.Errorf("mongo: leaseID 不能为空")
	}
	if _, err := e.col.DeleteOne(ctx, bson.M{"_id": "leader", "holder": leaseID}); err != nil {
		return fmt.Errorf("mongo: Release 失败: %w", err)
	}
	return nil
}
