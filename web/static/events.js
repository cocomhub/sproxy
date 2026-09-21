/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * events.js —— 文件变更事件流（SSE）前端模块。
 *
 * 职责（与 app-render.js 同隔离原则：纯函数部分与 DOM/网络完全隔离，node 直测）：
 *   1. parseSSE：SSE 文本流 → 事件数组（{id, data}；data 为 JSON 解析后的对象）
 *   2. isRefreshableAction：事件 action → 是否需要刷新文件列表（文件内容相关）
 *   3. nextCursor：游标推进（Last-Event-ID 重连回放）
 *   4. backoffDelay：断线重连指数退避（1s→2s→4s→…→30s 封顶）
 *   5. buildEventsUrl / buildEventsHeaders：连接 /api/events 的 URL 与认证头
 *
 * 浏览器挂全局 webEvents；Node 下 module.exports。顶层无 DOM/window 副作用。
 * 连接器（fetch 流式 SSE + 重连循环）由 app.js 负责（依赖页面凭据态与刷新钩子）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.webEvents = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // ---- SSE 解析（纯）----

  // parseSSE 解析 SSE 文本块 → [{id, data}]。
  // SSE 格式：若干事件块，每块由 id:/data: 行 + 空行分隔；注释行（: ...）忽略。
  // data 行尝试 JSON.parse（服务端 fileEvent JSON）；解析失败置 null（脏数据容错）。
  // 同一事件多 data 行按 SSE 规范拼接为一行后整体解析（本服务端单行，容错保留）。
  function parseSSE(raw) {
    if (typeof raw !== 'string' || raw.length === 0) return [];
    const events = [];
    // 空行分隔事件块；块内字段行按行处理。
    const blocks = raw.split(/\r?\n\r?\n/);
    for (const block of blocks) {
      if (!block || block.trim().length === 0) continue;
      let id = null;
      const dataLines = [];
      for (const line of block.split(/\r?\n/)) {
        if (line.startsWith(':')) continue; // 注释行（心跳）
        if (line.startsWith('id:')) {
          id = line.slice(3).trim();
        } else if (line.startsWith('data:')) {
          dataLines.push(line.slice(5).trim());
        }
      }
      if (id === null && dataLines.length === 0) continue;
      let data = null;
      if (dataLines.length > 0) {
        const joined = dataLines.join('\n');
        try {
          data = JSON.parse(joined);
        } catch (e) {
          data = null; // 非 JSON data：容错（不抛）
        }
      }
      events.push({ id: id === null ? null : id, data: data });
    }
    return events;
  }

  // ---- 动作过滤（纯）----

  // isRefreshableAction 判定事件 action 是否需要刷新文件列表。
  // 文件内容/结构相关动作 → true；非文件类（share 等）→ false。
  const REFRESHABLE_ACTIONS = ['upload', 'delete', 'rename', 'mkdir', 'rmdir', 'version'];

  function isRefreshableAction(action) {
    return typeof action === 'string' && REFRESHABLE_ACTIONS.indexOf(action) !== -1;
  }

  // ---- 游标推进（纯）----

  // nextCursor 推进 Last-Event-ID 游标：事件游标大于当前才更新（忽略乱序/非法值）。
  function nextCursor(current, eventId) {
    if (eventId === null || eventId === undefined || eventId === '') return current;
    const n = Number(eventId);
    if (!isFinite(n) || n <= 0) return current;
    return n > current ? n : current;
  }

  // ---- 重连退避（纯）----

  // backoffDelay 计算第 attempt 次重连的等待毫秒：2^attempt 秒，30s 封顶（初始 1s）。
  function backoffDelay(attempt) {
    const base = Math.pow(2, Math.max(0, attempt));
    return Math.min(base * 1000, 30000);
  }

  // ---- URL / 认证头（纯）----

  // buildEventsUrl 拼装 /api/events URL（owner 空则不携带参数）。
  function buildEventsUrl(owner) {
    if (!owner) return '/api/events';
    return '/api/events?owner=' + encodeURIComponent(owner);
  }

  // buildEventsHeaders 构造 SSE 请求头：
  //   - 无凭据 → {}（服务端 AllowInsecureLoopback 兜底 / 无认证场景直通）
  //   - 有凭据 → 委托 sclientSig.signHeader 生成 GET 签名头（认证态直连）
  // url 缺省 '/api/events'；调用方应传 buildEventsUrl(owner) 的完整路径（含 query），
  // 使 SproxySig canonical 的 path 段与请求实际 URL 一致（Go 按 r.URL.Path+Query 验签）。
  // sigLib 缺省取全局 sclientSig（浏览器）；测试可注入 stub。
  function buildEventsHeaders(ak, secret, entryID, sigLib, url) {
    if (!ak || !secret) return Promise.resolve({});
    const sig = sigLib || (typeof self !== 'undefined' && self.sclientSig) || null;
    if (!sig || typeof sig.signHeader !== 'function') return Promise.resolve({});
    const target = url || '/api/events';
    return sig.signHeader('GET', target, null, {
      ak: ak,
      secret: secret,
      entryID: entryID || '',
    }).then(function (auth) {
      return { Authorization: auth };
    }).catch(function () {
      return {}; // 签名失败不阻塞连接（服务端无凭据兜底/401 由重连处理）
    });
  }

  return {
    parseSSE: parseSSE,
    isRefreshableAction: isRefreshableAction,
    nextCursor: nextCursor,
    backoffDelay: backoffDelay,
    buildEventsUrl: buildEventsUrl,
    buildEventsHeaders: buildEventsHeaders,
    REFRESHABLE_ACTIONS: REFRESHABLE_ACTIONS,
  };
});
