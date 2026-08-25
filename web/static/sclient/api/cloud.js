/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * cloud.js —— sclient 领域 API：云端下载任务 / 组 / 归档。
 *
 * 闭包式工厂：api/cloud.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server handlers（cloud_download_handler.go / cloud_archive_handler.go）：
 *   - createDownload(url, filename)     POST /api/cloud/download
 *   - createBatch(urls)               POST /api/cloud/download/batch  urls=[{url,filename}]
 *   - listTasks(opts)                 GET  /api/cloud/tasks?status=&offset=&limit=
 *   - getTask(id)                    GET  /api/cloud/tasks/{id}
 *   - cancelTask(id)                 POST /api/cloud/tasks/{id}/cancel
 *   - deleteTask(id)                 DELETE /api/cloud/tasks/{id}
 *   - resumeTask(id, force)          POST /api/cloud/tasks/{id}/resume  {force}
 *   - archiveTask(id, archiveName)    POST /api/cloud/tasks/{id}/archive {archive_name}
 *   - archiveBatch(taskIds, name)     POST /api/cloud/archive  {task_ids, archive_name}
 *   - createGroup(name, urls)        POST /api/cloud/groups
 *   - listGroups(opts)               GET  /api/cloud/groups?status=&offset=&limit=
 *   - getGroup(id)                  GET  /api/cloud/groups/{id}（含子任务）
 *   - cancelGroup(id)               POST /api/cloud/groups/{id}/cancel
 *   - deleteGroup(id)               DELETE /api/cloud/groups/{id}
 *   - resumeGroup(id, force)        POST /api/cloud/groups/{id}/resume {force}
 *   - archiveGroup(id, archiveName)  POST /api/cloud/groups/{id}/archive {archive_name}
 *
 * 归一：列表类返回 {tasks,total} / {groups,total}（兼容旧裸数组：本领域直接
 * 返回 coreRequest body 解析后的对象；调用方如需兼容 Array 语义可检查 Array.isArray——
 * 服务端当前恒返回容器）。操作类返回 JSON 对象。归档类返回 {success,...}。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiCloud = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  const TE = new TextEncoder();

  function encodeJSON(obj) { return TE.encode(JSON.stringify(obj)); }

  // ---- 领域方法工厂 ----
  return function createCloudApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/cloud: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/cloud: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;
    const log = ctx.log;

    // 通用 JSON 请求（与 files.js 同形，闭包内自持实现避免循环引用）。
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

    function urlWithParams(base, params) {
      const parts = [];
      for (const k of Object.keys(params)) {
        if (params[k] === undefined || params[k] === null || params[k] === '') continue;
        parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(params[k])));
      }
      return parts.length ? base + '?' + parts.join('&') : base;
    }

    // ---- 任务 ----
    function createDownload(url, filename) {
      const data = { url: url };
      if (filename) data.filename = filename;
      return jsonRequest('POST', '/api/cloud/download', data);
    }

    function createBatch(urls) {
      return jsonRequest('POST', '/api/cloud/download/batch', { urls: urls });
    }

    function listTasks(opts) {
      const p = opts || {};
      return jsonRequest('GET', urlWithParams('/api/cloud/tasks', {
        status: p.status,
        offset: p.offset !== undefined ? String(p.offset) : undefined,
        limit: p.limit !== undefined ? String(p.limit) : undefined,
      }), undefined);
    }

    function getTask(id) {
      return jsonRequest('GET', '/api/cloud/tasks/' + encodeURIComponent(id), undefined);
    }

    function cancelTask(id) {
      return jsonRequest('POST', '/api/cloud/tasks/' + encodeURIComponent(id) + '/cancel', {});
    }

    function deleteTask(id) {
      return jsonRequest('DELETE', '/api/cloud/tasks/' + encodeURIComponent(id), undefined);
    }

    function resumeTask(id, force) {
      return jsonRequest('POST', '/api/cloud/tasks/' + encodeURIComponent(id) + '/resume', { force: force === true });
    }

    function archiveTask(id, archiveName) {
      const data = {};
      if (archiveName) data.archive_name = archiveName;
      return jsonRequest('POST', '/api/cloud/tasks/' + encodeURIComponent(id) + '/archive', data);
    }

    function archiveBatch(taskIds, archiveName) {
      const data = { task_ids: taskIds };
      if (archiveName) data.archive_name = archiveName;
      return jsonRequest('POST', '/api/cloud/archive', data);
    }

    // ---- 组 ----
    function createGroup(name, urls) {
      const data = { name: name, urls: urls };
      return jsonRequest('POST', '/api/cloud/groups', data);
    }

    function listGroups(opts) {
      const p = opts || {};
      return jsonRequest('GET', urlWithParams('/api/cloud/groups', {
        status: p.status,
        offset: p.offset !== undefined ? String(p.offset) : undefined,
        limit: p.limit !== undefined ? String(p.limit) : undefined,
      }), undefined);
    }

    function getGroup(id) {
      return jsonRequest('GET', '/api/cloud/groups/' + encodeURIComponent(id), undefined);
    }

    function cancelGroup(id) {
      return jsonRequest('POST', '/api/cloud/groups/' + encodeURIComponent(id) + '/cancel', {});
    }

    function deleteGroup(id) {
      return jsonRequest('DELETE', '/api/cloud/groups/' + encodeURIComponent(id), undefined);
    }

    function resumeGroup(id, force) {
      return jsonRequest('POST', '/api/cloud/groups/' + encodeURIComponent(id) + '/resume', { force: force === true });
    }

    function archiveGroup(id, archiveName) {
      const data = {};
      if (archiveName) data.archive_name = archiveName;
      return jsonRequest('POST', '/api/cloud/groups/' + encodeURIComponent(id) + '/archive', data);
    }

    return {
      createDownload,
      createBatch,
      listTasks,
      getTask,
      cancelTask,
      deleteTask,
      resumeTask,
      archiveTask,
      archiveBatch,
      createGroup,
      listGroups,
      getGroup,
      cancelGroup,
      deleteGroup,
      resumeGroup,
      archiveGroup,
    };
  };
});
