/* SPDX-License-Identifier: Apache-2.0 */
/*
 * events.test.js —— 文件变更事件流（SSE）前端纯函数单元测试。
 *
 * 运行：node --test web/static/events.test.js（已并入 make web-test）。
 *
 * 覆盖（全部纯函数，无 DOM/网络，node 直测）：
 *   - parseSSE：SSE 文本流 → 事件数组（id/data 字段、空行分隔、脏数据容错）
 *   - isRefreshableAction：事件类型 → 是否需要刷新文件列表
 *   - nextCursor：事件游标推进（含越界/非数字容错）
 *   - backoffDelay：指数退避（上限 30s，初始 1s）
 *   - buildEventsUrl：owner → /api/events URL
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const ev = require(path.join(__dirname, 'events.js'));

test('parseSSE 解析标准 SSE 事件（id + data + 空行）', () => {
  const raw = 'id: 3\ndata: {"cursor":3,"action":"upload","owner":"anonymous","rel":"a.txt","size":12}\n\nid: 4\ndata: {"cursor":4,"action":"delete","owner":"anonymous","rel":"b.txt"}\n\n';
  const events = ev.parseSSE(raw);
  assert.strictEqual(events.length, 2);
  assert.deepStrictEqual(events[0], { id: '3', data: { cursor: 3, action: 'upload', owner: 'anonymous', rel: 'a.txt', size: 12 } });
  assert.deepStrictEqual(events[1], { id: '4', data: { cursor: 4, action: 'delete', owner: 'anonymous', rel: 'b.txt' } });
});

test('parseSSE 容忍注释行与空事件（: connected 心跳）', () => {
  const raw = ': connected\n\nid: 1\ndata: {"cursor":1,"action":"mkdir","owner":"anonymous","rel":"d"}\n\n';
  const events = ev.parseSSE(raw);
  assert.strictEqual(events.length, 1);
  assert.strictEqual(events[0].id, '1');
  assert.strictEqual(events[0].data.action, 'mkdir');
});

test('parseSSE 对非 JSON data 不抛错（脏数据容错）', () => {
  const raw = 'id: 9\ndata: not-json\n\n';
  const events = ev.parseSSE(raw);
  assert.strictEqual(events.length, 1);
  assert.strictEqual(events[0].id, '9');
  assert.strictEqual(events[0].data, null);
});

test('isRefreshableAction 文件内容相关动作需刷新', () => {
  for (const a of ['upload', 'delete', 'rename', 'mkdir', 'rmdir', 'version']) {
    assert.ok(ev.isRefreshableAction(a), a + ' 应触发刷新');
  }
  assert.strictEqual(ev.isRefreshableAction('share'), false);
  assert.strictEqual(ev.isRefreshableAction(''), false);
  assert.strictEqual(ev.isRefreshableAction(null), false);
});

test('nextCursor 推进游标（忽略越界/非数字）', () => {
  assert.strictEqual(ev.nextCursor(0, 5), 5);
  assert.strictEqual(ev.nextCursor(3, 5), 5);
  assert.strictEqual(ev.nextCursor(7, 5), 7); // 事件游标小于已存，保持
  assert.strictEqual(ev.nextCursor(0, 0), 0);
  assert.strictEqual(ev.nextCursor(0, NaN), 0);
  assert.strictEqual(ev.nextCursor(0, undefined), 0);
  assert.strictEqual(ev.nextCursor(0, null), 0);
});

test('backoffDelay 指数退避（初始 1s，上限 30s）', () => {
  assert.strictEqual(ev.backoffDelay(0), 1000);
  assert.strictEqual(ev.backoffDelay(1), 2000);
  assert.strictEqual(ev.backoffDelay(2), 4000);
  assert.strictEqual(ev.backoffDelay(3), 8000);
  assert.strictEqual(ev.backoffDelay(4), 16000);
  assert.strictEqual(ev.backoffDelay(5), 30000); // 封顶
  assert.strictEqual(ev.backoffDelay(99), 30000);
});

test('buildEventsUrl 拼装 owner 参数', () => {
  assert.strictEqual(ev.buildEventsUrl('anonymous'), '/api/events?owner=anonymous');
  assert.strictEqual(ev.buildEventsUrl(''), '/api/events');
  assert.strictEqual(ev.buildEventsUrl(null), '/api/events');
  assert.strictEqual(ev.buildEventsUrl('a/b'), '/api/events?owner=a%2Fb');
});

test('buildEventsHeaders 带凭据时生成签名头，无凭据空对象', async () => {
  // 无凭据：返回空对象（服务端 AllowInsecureLoopback 兜底 / 无认证场景直通）。
  const none = await ev.buildEventsHeaders('', '', '');
  assert.deepStrictEqual(none, {});
  // 有凭据：委托 sclientSig.signHeader 生成 GET 签名头（Authorization）。
  const sigStub = {
    signHeader: async (method, pathWithQuery, body, opts) => {
      assert.strictEqual(method, 'GET');
      assert.strictEqual(pathWithQuery, '/api/events?owner=anonymous');
      assert.strictEqual(body, null);
      assert.strictEqual(opts.ak, 'ak-mesh-abc');
      assert.strictEqual(opts.secret, 'sk-secret');
      return 'SproxySig v=2 ak=ak-mesh-abc ts=1 exp=2 nonce=n body_sha256=b sig=s';
    },
  };
  const h = await ev.buildEventsHeaders('ak-mesh-abc', 'sk-secret', '', sigStub, '/api/events?owner=anonymous');
  assert.strictEqual(h.Authorization, 'SproxySig v=2 ak=ak-mesh-abc ts=1 exp=2 nonce=n body_sha256=b sig=s');
});
