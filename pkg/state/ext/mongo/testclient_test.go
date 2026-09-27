// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mongo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// testClient 是测试专用 mongo 客户端（每用例独立连接池——禁止共享包级 client，
// 与测试网络客户端隔离硬规则同款）。
type testClient struct {
	URI string
	cli *mongo.Client
}

// newTestClient 连接 mongo 并 Ping（5s 超时）；失败返回 error（requireMongo 转 t.Skip）。
func newTestClient(t *testing.T, uri, db string) (*testClient, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := cli.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = cli.Disconnect(context.Background())
		return nil, err
	}
	return &testClient{URI: uri, cli: cli}, nil
}

// uniqueCollection 返回带随机后缀的集合名（并行用例隔离），并在测试结束删除。
func uniqueCollection(t *testing.T, c *testClient, db, prefix string) string {
	t.Helper()
	col := fmt.Sprintf("%s_%s_%d", prefix, t.Name(), time.Now().UnixNano())
	// t.Name() 含 "/"（subtest）——只取测试函数名部分。
	for _, ch := range col {
		if ch == '/' || ch == ' ' {
			col = fmt.Sprintf("%s_%s_%d", prefix, sanitize(t.Name()), time.Now().UnixNano())
			break
		}
	}
	_ = c.cli.Database(db).Collection(col).Drop(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.cli.Database(db).Collection(col).Drop(ctx)
	})
	return col
}

// sanitize 把 t.Name() 中的非法字符替换为下划线（集合名只允许 [A-Za-z0-9_.$]）。
func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '$':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// expireLease 把 leader 租约文档的 expires_at 改为过去时间（模拟 TTL 过期，
// 不依赖真实 TTL index 等待）。
func expireLease(uri, db, col string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return err
	}
	defer func() { _ = cli.Disconnect(context.Background()) }()
	_, err = cli.Database(db).Collection(col).UpdateOne(ctx,
		bson.M{"_id": "leader"},
		bson.M{"$set": bson.M{"expires_at": time.Now().Add(-time.Hour)}},
	)
	return err
}
