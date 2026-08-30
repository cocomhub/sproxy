/* SPDX-License-Identifier: Apache-2.0 */
/*
 * sync.test.js —— 文件同步（sync_task）领域 API + 前端纯函数单元测试。
 *
 * 运行：node --test web/static/sync.test.js（已并入 make web-test）。
 *
 * 覆盖：
 *   - sc.sync 领域 API（sclient/api/sync.js）的 URL/方法/body 映射（mock coreRequest）
 *   - app-render.js 的 syncStatusText / buildSyncRowMeta 纯函数
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const apiIndex = require(path.join(__dirname, 'sclient/api/index.js'));
const apiUtil = require(path.join(__dirname, 'sclient/util.js'));
const r = require(path.join(__dirname, 'app-render.js'));

// ---- mock 传输层（与 sclient.test.js 同形） ----
// _throwStatus 模拟真实 coreRequest（transport.js directRun/tunnelRun）对非 2xx 抛
// SclientError（带 status）的语义（审查 M-2）：让错误路径测试能断言前端按 status 分支。
function makeMockCore(results) {
  const calls = [];
  const fn = async (method, pathArg, opts) => {
    calls.push({ method, path: pathArg, opts: opts || {} });
    const res = results && results.length ? results.shift() : { status: 200, headers: {}, body: new Uint8Array(0) };
    if (res && res._throw) throw (res.err instanceof Error ? res.err : new Error(String(res.err)));
    if (res && res._throwStatus) {
      const err = new Error('请求失败（HTTP ' + res._throwStatus + '）');
      err.status = res._throwStatus; // 对齐 transport.js SclientError('E_SERVER', msg, status)
      throw err;
    }
    return res;
  };
  fn.calls = calls;
  return fn;
}

function jsonBody(body) {
  return body == null ? {} : JSON.parse(new TextDecoder().decode(body));
}

function okResp(obj) {
  return { status: 200, headers: {}, body: new TextEncoder().encode(JSON.stringify(obj)) };
}

function makeApi(core) {
  return apiIndex.createApi({
    coreRequest: core,
    config: { chunkThreshold: 8 * 1024 * 1024 },
    log: undefined,
    crypto: undefined,
    util: apiUtil,
  });
}

// ---- sc.sync 领域 API 映射 ----

test('sc.sync 任务映射（list/get/create/cancel/delete 的 URL/方法/body）', async () => {
  const core = makeMockCore([
    okResp({ success: true, tasks: [{ id: 's1', status: 'pending' }] }),
    okResp({ id: 's2', direction: 'push', status: 'syncing' }),
    okResp({ id: 's3', status: 'completed' }),
    okResp({ status: 'cancelled' }),
    okResp({ status: 'deleted' }),
  ]);
  const api = makeApi(core);

  const list = await api.sync.listTasks();
  assert.strictEqual(core.calls[0].method, 'GET');
  assert.strictEqual(core.calls[0].path, '/api/sync/tasks');
  assert.ok(Array.isArray(list.tasks) && list.tasks.length === 1, '列表返回 {tasks:[...]}');
  assert.strictEqual(list.tasks[0].id, 's1');

  await api.sync.getTask('s2');
  assert.strictEqual(core.calls[1].method, 'GET');
  assert.strictEqual(core.calls[1].path, '/api/sync/tasks/s2');

  await api.sync.createTask({
    direction: 'push', remote: 'r1', src: 'a', dst: 'b', recursive: true, conflict_policy: 'skip',
  });
  assert.strictEqual(core.calls[2].method, 'POST');
  assert.strictEqual(core.calls[2].path, '/api/sync/tasks');
  assert.strictEqual(core.calls[2].opts.headers['Content-Type'], 'application/json');
  assert.deepStrictEqual(jsonBody(core.calls[2].opts.bodyBytes), {
    direction: 'push', remote: 'r1', src: 'a', dst: 'b', recursive: true, conflict_policy: 'skip',
  });

  await api.sync.cancelTask('s4');
  assert.strictEqual(core.calls[3].method, 'POST');
  assert.strictEqual(core.calls[3].path, '/api/sync/tasks/s4/cancel');

  await api.sync.deleteTask('s5');
  assert.strictEqual(core.calls[4].method, 'DELETE');
  assert.strictEqual(core.calls[4].path, '/api/sync/tasks/s5');
});

test('sc.sync.createTask 缺省可选字段省略（recursive/conflict_policy 未传不发 undefined/空串）', async () => {
  const core = makeMockCore([okResp({ id: 'x', status: 'pending' })]);
  const api = makeApi(core);
  await api.sync.createTask({ direction: 'pull', remote: 'r2', src: 's' });
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), { direction: 'pull', remote: 'r2', src: 's' });
});

test('sc.sync 携带全部可选字段（include/exclude/sync_empty_dirs/follow_symlinks）', async () => {
  const core = makeMockCore([okResp({ id: 'y', status: 'pending' })]);
  const api = makeApi(core);
  await api.sync.createTask({
    direction: 'push', remote: 'r3', src: 'a', dst: 'b',
    recursive: false, include: ['*.go'], exclude: ['*.tmp'],
    conflict_policy: 'overwrite', sync_empty_dirs: true, follow_symlinks: false,
  });
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), {
    direction: 'push', remote: 'r3', src: 'a', dst: 'b',
    recursive: false, include: ['*.go'], exclude: ['*.tmp'],
    conflict_policy: 'overwrite', sync_empty_dirs: true, follow_symlinks: false,
  });
});

// ---- app-render sync 纯函数 ----

test('syncStatusText 映射 sync 状态（含 syncing），未知回落原值', () => {
  assert.strictEqual(r.syncStatusText('pending'), '⏳ 等待中');
  assert.strictEqual(r.syncStatusText('syncing'), '🔄 同步中');
  assert.strictEqual(r.syncStatusText('completed'), '✅ 已完成');
  assert.strictEqual(r.syncStatusText('failed'), '❌ 失败');
  assert.strictEqual(r.syncStatusText('cancelled'), '🚫 已取消');
  assert.strictEqual(r.syncStatusText('weird'), 'weird');
  assert.strictEqual(r.syncStatusText(''), '');
  assert.strictEqual(r.syncStatusText(undefined), undefined);
});

test('buildSyncRowMeta 字节进度优先：title/direction/progressText/hasBytes', () => {
  const m = r.buildSyncRowMeta({
    filename: 'a', src: 'a', dst: 'b', loaded: 10, total: 100,
    meta: { sync: { direction: 'push', files_done: 1, files_total: 3 } },
  });
  assert.strictEqual(m.title, 'a');
  assert.strictEqual(m.direction, 'push');
  assert.strictEqual(m.src, 'a');
  assert.strictEqual(m.dst, 'b');
  assert.strictEqual(m.hasBytes, true);
  assert.strictEqual(m.bytesPct, 10);
  assert.strictEqual(m.hasFiles, true);
  assert.strictEqual(m.filesDone, 1);
  assert.strictEqual(m.filesTotal, 3);
  assert.ok(m.progressText.indexOf('10 B / 100 B') >= 0, '字节进度文案: ' + m.progressText);
});

test('buildSyncRowMeta 无字节时回落文件进度；标题回退 "同步任务"', () => {
  const m = r.buildSyncRowMeta({ id: 's', kind: 'sync_task', meta: { sync: { files_done: 2, files_total: 5 } } });
  assert.strictEqual(m.title, '同步任务');
  assert.strictEqual(m.hasBytes, false);
  assert.strictEqual(m.hasFiles, true);
  assert.strictEqual(m.bytesPct, 0);
  assert.strictEqual(m.filesPct, 40);
  assert.ok(m.progressText.indexOf('2/5') >= 0, '文件进度文案: ' + m.progressText);
  assert.ok(m.progressText.indexOf('40%') >= 0);
});

test('buildSyncRowMeta 全缺省：不抛错，progressText 空串', () => {
  const m = r.buildSyncRowMeta(null);
  assert.strictEqual(m.title, '同步任务');
  assert.strictEqual(m.hasBytes, false);
  assert.strictEqual(m.hasFiles, false);
  assert.strictEqual(m.progressText, '');
  const m2 = r.buildSyncRowMeta({ id: 'x' });
  assert.strictEqual(m2.title, '同步任务');
});

// 审查 M-2：错误路径（非 2xx）抛带 status 的错误（对齐 transport.js SclientError 语义），
// 供前端 refreshSyncTasks/createSyncTask 按 status 分支（I-1/M-3 回归防线）。
test('sc.sync 错误路径：非 2xx 抛带 status 的错误', async () => {
  const core = makeMockCore([
    { _throwStatus: 400, status: 400 }, // list：sync 未配置
    { _throwStatus: 404, status: 404 }, // cancel：任务不存在
    { _throwStatus: 400, status: 400 }, // create：参数有误（remote 未配置等）
    { _throwStatus: 507, status: 507 }, // create：存储不足
  ]);
  const api = makeApi(core);

  await assert.rejects(api.sync.listTasks(), (e) => e.status === 400, 'list 400 应抛带 status 错误');
  await assert.rejects(api.sync.cancelTask('x'), (e) => e.status === 404, 'cancel 404 应抛带 status 错误');
  await assert.rejects(
    api.sync.createTask({ direction: 'push', remote: 'nope', src: 'a' }),
    (e) => e.status === 400,
    'create 400 应抛带 status 错误'
  );
  await assert.rejects(
    api.sync.createTask({ direction: 'pull', remote: 'full', src: 'a' }),
    (e) => e.status === 507,
    'create 507 应抛带 status 错误'
  );
});
