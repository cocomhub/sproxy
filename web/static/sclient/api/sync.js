/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * sync.js —— sclient 领域 API：文件同步任务（服务端 SyncManager /api/sync/tasks）。
 *
 * 闭包式工厂：api/sync.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server handlers（sync_handler.go / syncmgr）：
 *   - listTasks()                    GET    /api/sync/tasks              → {success, tasks:[SyncTaskMeta]}
 *   - getTask(id)                    GET    /api/sync/tasks/{id}         → SyncTask
 *   - createTask(data)               POST   /api/sync/tasks              → SyncTask（201 新建 / 200 去重复用）
 *   - cancelTask(id)                 POST   /api/sync/tasks/{id}/cancel
 *   - deleteTask(id)                 DELETE /api/sync/tasks/{id}
 *
 * createTask 的 data 字段（对齐 syncmgr.CreateRequest）：direction(push|pull)/remote/src/dst/
 * recursive/include/exclude/conflict_policy(skip|overwrite|lww|conflict_rename)/
 * sync_empty_dirs/follow_symlinks。未提供的可选字段不写入 body（undefined/空串不发）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiSync = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  const TE = new TextEncoder();

  function encodeJSON(obj) { return TE.encode(JSON.stringify(obj)); }

  return function createSyncApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/sync: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/sync: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;

    async function jsonRequest(method, path, data, opts) {
      const o = opts || {};
      const headers = {};
      let bodyBytes = null;
      if (data !== undefined && data !== null) {
        bodyBytes = encodeJSON(data);
        headers['Content-Type'] = 'application/json';
      }
      if (o.headers) {
        for (const k of Object.keys(o.headers)) headers[k] = o.headers[k];
      }
      if (o.bodyBytes) bodyBytes = o.bodyBytes;
      const res = await coreRequest(method, path, { headers: headers, bodyBytes: bodyBytes });
      const parsed = util.decodeJSON(res.body);
      return Object.assign({ status: res.status, headers: res.headers }, parsed);
    }

    function listTasks() {
      return jsonRequest('GET', '/api/sync/tasks', undefined);
    }

    function getTask(id) {
      return jsonRequest('GET', '/api/sync/tasks/' + encodeURIComponent(id), undefined);
    }

    function createTask(data) {
      const d = data || {};
      const body = { direction: d.direction, remote: d.remote };
      if (d.src !== undefined) body.src = d.src;
      if (d.dst !== undefined) body.dst = d.dst;
      if (d.recursive !== undefined) body.recursive = !!d.recursive;
      if (Array.isArray(d.include) && d.include.length) body.include = d.include;
      if (Array.isArray(d.exclude) && d.exclude.length) body.exclude = d.exclude;
      if (d.conflict_policy !== undefined) body.conflict_policy = d.conflict_policy;
      if (d.sync_empty_dirs !== undefined) body.sync_empty_dirs = !!d.sync_empty_dirs;
      if (d.follow_symlinks !== undefined) body.follow_symlinks = !!d.follow_symlinks;
      return jsonRequest('POST', '/api/sync/tasks', body);
    }

    function cancelTask(id) {
      return jsonRequest('POST', '/api/sync/tasks/' + encodeURIComponent(id) + '/cancel', {});
    }

    function deleteTask(id) {
      return jsonRequest('DELETE', '/api/sync/tasks/' + encodeURIComponent(id), undefined);
    }

    return {
      listTasks,
      getTask,
      createTask,
      cancelTask,
      deleteTask,
    };
  };
});
