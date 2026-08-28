// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 下载管理管线：把一次性的全量 GET /download 改为分块拉取 + IndexedDB 缓存 +
// 断点续传（暂停/取消/恢复），恢复时先 stat HEAD 校验收到的块是否仍与服务端一致。
//
// global（调用点运行期解引用，加载序安全同 upload.js）：
//   transferStore（transfer-store.js）——块缓存 + TransferItem 列表（缺省 store）
//   sclientTransport（sclient/transport.js）——coreRequest 归一接口
//
// 隔离原则（与 upload.js 一致）：纯函数不碰 DOM/全局，Node 可 require 直测；
// createDownloadManager({store, transport}) 注入式（测试传内存 mock）。
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.downloadMgr = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  const BASE = 4 * 1024 * 1024;
  const MAX = 64 * 1024 * 1024;
  const PARALLEL = 3;

  // ---- 纯函数 ----

  // download 分块请求路径（encodeURIComponent 完整 filename；offset/length 数值）。
  function chunkUrlFor(filename, offset, length) {
    return '/download/chunk?filename=' + encodeURIComponent(filename) + '&offset=' + offset + '&length=' + length;
  }

  // 分块大小协商（对齐 sclient/api/files.js calcChunkSize）。
  function calcChunkSize(fileSize) {
    let cs = BASE;
    while (cs * 512 < fileSize && cs < MAX) cs *= 2;
    return Math.min(cs, MAX);
  }

  // 块数：totalBytes/chunkSize 向上取整。非法输入 → 0。
  function chunkCount(totalBytes, chunkSize) {
    const t = Number(totalBytes);
    const c = Number(chunkSize);
    if (!isFinite(t) || t < 0) return 0;
    if (!isFinite(c) || c <= 0) return 0;
    return Math.ceil(t / c);
  }

  // 全量索引 [0, totalChunks)。非法/负/NaN → []。
  function allChunkIndices(totalChunks) {
    const n = Math.floor(Number(totalChunks));
    if (!isFinite(n) || n < 0) return [];
    const out = [];
    for (let i = 0; i < n; i++) out.push(i);
    return out;
  }

  // 缺失块列表：bitmap falsy/缺位视为未下载。
  function missingChunkList(bitmap, expectedTotal) {
    const n = Math.floor(Number(expectedTotal));
    if (!isFinite(n) || n < 0) return [];
    const bm = Array.isArray(bitmap) ? bitmap : [];
    const out = [];
    for (let i = 0; i < n; i++) if (!bm[i]) out.push(i);
    return out;
  }

  // 大小写兼容的响应头取值（flat map 或数组值）。
  function headerValue(headers, key) {
    if (!headers || !key) return '';
    const direct = headers[key] || headers[key.toLowerCase()] || headers[key.toUpperCase()];
    if (Array.isArray(direct)) return direct[0] || '';
    if (direct === undefined || direct === null) return '';
    return String(direct);
  }

  // stat 响应（{headers}）归一为 {size, mtime, checksum}。
  function serverMetaFromStat(res) {
    const h = (res && res.headers) ? res.headers : {};
    const sizeN = Number(headerValue(h, 'X-File-Size'));
    return {
      size: isFinite(sizeN) && sizeN > 0 ? sizeN : 0,
      mtime: headerValue(h, 'X-File-MTime'),
      checksum: headerValue(h, 'X-File-Checksum'),
    };
  }

  // 恢复校验：size 必须匹配；双方都有 mtime → 比 mtime；缺一侧 mtime → 比 checksum
  // （有则比，无则视为匹配——无法比对不拒绝续传）；双缺 → true。
  function metaMatches(stored, server) {
    const a = stored || {};
    const b = server || {};
    const an = Number(a.size);
    if (isFinite(an) && an > 0 && an !== Number(b.size)) return false;
    const sm = a.mtime || '';
    const bm = b.mtime || '';
    if (sm && bm) return sm === bm;
    if (!sm && !bm) return true;
    const ac = a.checksum ? a.checksum.toLowerCase() : '';
    const bc = b.checksum ? String(b.checksum).toLowerCase() : '';
    if (ac || bc) return ac === bc;
    return true;
  }

  // FNV-1a 32 生成稳定 item id（下载唯一性；无需加密强度）。
  function idFor(filename) {
    let h = 2166136261;
    const s = String(filename);
    for (let i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i);
      h = Math.imul(h, 16777619);
    }
    return 'dl-' + (h >>> 0).toString(16).padStart(8, '0');
  }

  function parseHooks(hooks) {
    const h = (hooks && typeof hooks === 'object') ? hooks : {};
    return {
      onProgress: typeof h.onProgress === 'function' ? h.onProgress : null,
      onVerify: typeof h.onVerify === 'function' ? h.onVerify : null,
      onComplete: typeof h.onComplete === 'function' ? h.onComplete : null,
      onMismatch: typeof h.onMismatch === 'function' ? h.onMismatch : null,
      onError: typeof h.onError === 'function' ? h.onError : null,
    };
  }

  function metaFromStat(stat) {
    return {
      mtimeNano: (stat && stat.mtime) || '',
      checksum: (stat && stat.checksum) || '',
    };
  }
  function copyMeta(item) {
    const m = (item && item.meta) || {};
    return Object.assign({}, m, { mtimeNano: m.mtimeNano !== undefined ? m.mtimeNano : '', checksum: m.checksum || '' });
  }

  let _defaultStore = null;
  function defaultStore() {
    if (!_defaultStore && typeof transferStore !== 'undefined' && transferStore.createTransferStore) {
      _defaultStore = transferStore.createTransferStore({});
    }
    return _defaultStore || noopStore();
  }
  function noopStore() {
    return {
      loadItems() { return []; }, upsertItem() {}, removeItem() {},
      saveChunk() {}, loadChunk() { return Promise.resolve(null); },
      listChunkCount() { return Promise.resolve(0); },
      deleteChunkRange() { return Promise.reject(new Error('store unavailable')); },
    };
  }
  function defaultTransport() {
    if (typeof sclientTransport !== 'undefined' && sclientTransport.coreRequest) return sclientTransport;
    return { coreRequest: null };
  }

  // createDownloadManager 是唯一入口；store/transport 可注入（测试用内存 mock）。
  function createDownloadManager(opts) {
    opts = opts || {};
    const store = opts.store || defaultStore();
    const transport = opts.transport || defaultTransport();
    return new Manager(store, transport);
  }

  function Manager(store, transport) {
    const self = this;
    self._store = store;
    self._transport = transport;
    self._running = {}; // itemId → { aborter, state }（进行中会话）

    self.findItem = function (idOrItem) {
      const list = store.loadItems();
      for (let i = 0; i < list.length; i++) {
        const it = list[i];
        if (it.id === idOrItem || it === idOrItem) return it;
      }
      return null;
    };
    self.getItem = function (id) { return self.findItem(id); };

    function currentItem(item) {
      return self.findItem(item.id) || item;
    }
    function persistItem(item, patch) {
      const cur = currentItem(item);
      const next = Object.assign({}, cur, patch);
      // 每次写回统一补齐 meta 必备字段，防止存档漂移缺字段。
      next.meta = Object.assign({}, copyMeta(cur), next.meta || {});
      store.upsertItem(next);
      return next;
    }

    // ---- 拉取泵（新建/恢复/强制共用）----
    // run(item, filename, chunkSize, missing, hooks, force)：并发 PARALLEL 拉缺失块，
    // 每块成功写 IDB + upsertItem 进度；全块就绪 → 合并 Blob → onVerify → onComplete →
    // 清块缓存 → completed。分块失败 → status failed 写回原因（保留已拉块供恢复）。
    function run(item, filename, chunkSize, missing, hooks, force) {
      const existing = self._running[item.id];
      if (existing && existing.state === 'running') return Promise.reject(new Error('该下载已在处理中'));
      const aborter = new AbortController();
      const session = { aborter: aborter, state: 'running' };
      self._running[item.id] = session;

      const total = Number(item.totalSize) > 0 ? Number(item.totalSize) : Number(item.total);
      const totalChunks = chunkCount(total, chunkSize);
      const bitmap = force ? new Array(totalChunks).fill(0)
        : sliceToLen(item.meta && item.meta.chunksBitmap, totalChunks);
      let loaded = force ? 0 : (Number(item.loaded) > 0 ? Number(item.loaded) : 0);
      let lastChunkHeaders = null;
      let pending = missing.slice();

      function persistRunning() {
        const base = currentItem(item);
        const next = Object.assign({}, base, {
          status: 'downloading',
          loaded: loaded,
          meta: Object.assign(copyMeta(base), {
            chunkSize: chunkSize, totalChunks: totalChunks, chunksBitmap: bitmap.slice(),
          }),
        });
        store.upsertItem(next);
        return next;
      }
      persistRunning();

      function recordChunk(idx, data, recSize) {
        bitmap[idx] = 1;
        loaded += recSize;
        persistItem(item, {
          loaded: loaded,
          status: 'downloading',
          meta: Object.assign(copyMeta(currentItem(item)), {
            chunksBitmap: bitmap.slice(), totalChunks: totalChunks, chunkSize: chunkSize, chunkIndex: idx,
          }),
        });
        if (typeof store.saveChunk === 'function') {
          store.saveChunk(item.id, idx, data, recSize).catch(function () { /* 尽力而为（块缓存可重写） */ });
        }
      }

      function stopped() { return session.state === 'paused' || session.state === 'cancelled'; }

      function fetchOne(idx) {
        const start = idx * chunkSize;
        const len = Math.min(chunkSize, total - start);
        if (len <= 0 || stopped()) return Promise.resolve();
        const controller = new AbortController();
        const onAbort = function () { controller.abort(); };
        const sig = session.aborter.signal;
        sig.addEventListener('abort', onAbort, { once: true });
        const cleanup = function () { sig.removeEventListener('abort', onAbort); };
        // 暂停/取消：把 abort（或未发出的请求）转化为已决值，让 pump 的 Promise.all 等齐。
        const proceed = function (err) {
          if (stopped()) return Promise.resolve();
          if (err) return Promise.reject(err);
          return undefined;
        };
        return Promise.resolve()
          .then(function () {
            if (!transport || typeof transport.coreRequest !== 'function') throw new Error('传输层不可用');
            return transport.coreRequest('GET', chunkUrlFor(filename, start, len), { headers: {}, signal: controller.signal });
          })
          .then(function (res) {
            if (stopped()) return; // abort/竞态后仍到达 → 不落块
            if (!res || res.status !== 200) throw new Error('分块下载失败: HTTP ' + ((res && res.status) || '?'));
            lastChunkHeaders = (res && res.headers) || null;
            const raw = res.body;
            const bytes = raw instanceof Uint8Array ? raw : new Uint8Array((raw && raw.byteLength) ? raw : 0);
            // 服务端按 length 截断，实际返回可能短于 len（文件尾部/边界）——以实际字节为准。
            const block = bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
            recordChunk(idx, block, bytes.byteLength);
          }, function (err) { return proceed(err); })
          .then(function () { return proceed(); }).finally(function () { cleanup(); });
      }

      // pump：并发拉取。停止（pause/cancel）后不再调度新块；在途块由 fetchOne 的
      // abort→proceed 短路收尾（Promise.all 等齐即退）。
      function pump() {
        if (stopped() || pending.length === 0) return Promise.resolve();
        const active = [];
        while (pending.length && active.length < PARALLEL) active.push(fetchOne(pending.shift()));
        return Promise.all(active).then(pump);
      }

      function readAllChunks() {
        const gets = [];
        for (let i = 0; i < totalChunks; i++) {
          // loadChunk 返回记录 {itemId, chunkIndex, data:ArrayBuffer, size}——只取 .data
          // （直接给 Blob 会把记录当字符串物化，长度错、内容错）。
          gets.push(Promise.resolve(store.loadChunk(item.id, i)).then(function (rec) {
            return rec ? rec.data : null;
          }).catch(function () { return null; }));
        }
        return Promise.all(gets);
      }

      function isAbort(err) { return !!(err && (err.code === 'E_ABORT' || err.name === 'AbortError')); }

      // 完成/失败/中断统一收尾：清 _running 会话（pause/cancel 已删时幂等）。
      function finish() {
        if (session.state !== 'paused' && session.state !== 'cancelled') session.state = 'finished';
        if (self._running[item.id] === session) delete self._running[item.id];
      }

      function assembleAndSave() {
        return readAllChunks().then(function (chunks) {
          if (chunks.some(function (c) { return !c; })) {
            throw new Error('部分分块缺失（缓存不完整），无法合并');
          }
          return new Blob(chunks, { type: 'application/octet-stream' });
        }).then(function (blob) {
          if (hooks.onVerify) {
            return Promise.resolve(hooks.onVerify(blob, headerValue(lastChunkHeaders, 'X-File-Checksum'))).then(function (ok) {
              if (ok === false) {
                const err = new Error('下载内容校验失败');
                err.skipComplete = true;
                throw err;
              }
              return blob;
            });
          }
          return blob;
        }).then(function (blob) {
          // 取消竞态复查必须覆盖写回全链：deleteChunkRange（异步 IDB）完成后到
          // persistItem/onComplete 之间仍有被 cancel（removeItem）穿插的窗口——若只复查
          // 一次于开头（pump 后），cancel 落在该窗口会把已删 item 复活为 completed 并触发
          // onComplete（R2）。故此处两层复查：deleteChunkRange 前复查一次 + 完成后、
          // persistItem 前再复查一次，且 onComplete 发射前亦兜底复查。任一停下即静默中止。
          if (stopped()) return;
          return Promise.resolve(store.deleteChunkRange(item.id)).catch(function () { /* 清缓存失败不阻断 */ }).then(function () {
            // deleteChunkRange 完成（IDB 异步落定）后、写回前二次复查——覆盖 cancel 落在
            // delete 进行中的窗口：此时 item 已被 removeItem，恢复写 completed 属复活。
            if (stopped()) return;
            persistItem(item, {
              status: 'completed',
              loaded: total,
              totalSize: total,
              total: total,
              meta: { chunksBitmap: bitmap.slice(), totalChunks: totalChunks, chunkSize },
            });
            if (stopped()) return;
            if (hooks.onComplete) hooks.onComplete(blob, item.filename || filename);
          });
        }).catch(function (err) {
          // 完成前失败（含 onVerify false / 缺块）：置 failed 写回错误原因，保留缓存供恢复。
          if (isAbort(err) || stopped()) return; // 暂停/取消已由 pause/cancel 写状态；跳过终态写回
          persistItem(item, { status: 'failed', failedAt: Date.now(), lastError: (err && err.message) ? err.message : String(err) });
          if (hooks.onError && !err.skipComplete) hooks.onError(err);
        });
      }

      return pump().then(function () {
        if (stopped()) return;
        if (bitmapReady(bitmap, totalChunks) && loaded >= total) return assembleAndSave();
        persistItem(item, { status: 'failed', failedAt: Date.now(), lastError: '下载未完成（部分块缺失）' });
        return undefined;
      }).catch(function (err) {
        if (isAbort(err) || stopped()) return; // 暂停/取消已由 pause/cancel 写状态
        const it = currentItem(item);
        if (it && !(err && err.skipComplete)) {
          persistItem(it, { status: 'failed', failedAt: Date.now(), lastError: (err && err.message) ? err.message : String(err) });
        }
        if (hooks.onError && !(err && err.skipComplete)) hooks.onError(err);
        return undefined;
      }).then(function (r) {
        finish();
        return r;
      }, function (e) {
        finish();
        throw e;
      });
    }

    function bitmapReady(bitmap, totalChunks) {
      if (totalChunks === 0) return true;
      for (let i = 0; i < totalChunks; i++) if (!bitmap[i]) return false;
      return true;
    }
    function sliceToLen(arr, n) {
      const out = new Array(n).fill(0);
      if (Array.isArray(arr)) for (let i = 0; i < Math.min(arr.length, n); i++) out[i] = arr[i] ? 1 : 0;
      return out;
    }

    // ---- 公开 API ----

    // startDownload(filename, stat, hooks)：新建下载 item → 全量分块拉取。stat 需
    // {size, mtime?, checksum?}（来自 sc.files.stat 或文件行元信息）。
    self.startDownload = function (filename, stat, hooks) {
      const h = parseHooks(hooks);
      const total = Math.floor(Number((stat && (Number(stat.size) > 0 ? stat.size : stat.totalSize)) || 0));
      const chunkSize = calcChunkSize(total);
      const totalChunks = chunkCount(total, chunkSize);
      const item = {
        id: idFor(filename),
        kind: 'download',
        filename: filename,
        status: 'downloading',
        loaded: 0,
        total: total,
        totalSize: total,
        meta: Object.assign(metaFromStat(stat), {
          chunkSize: chunkSize, totalChunks: totalChunks,
          chunksBitmap: new Array(totalChunks).fill(0),
        }),
      };
      store.upsertItem(item);
      return run(item, filename, chunkSize, allChunkIndices(totalChunks), h, false);
    };

    // resumeDownload(idOrItem, hooks)：先 stat HEAD 比对 size/mtime/checksum；匹配只拉
    // 缺失块；不匹配 onMismatch（不改状态，供 UI 提示可强制重下）并返回 {mismatched:true}。
    // hooks.force=true → 跳过 stat 强制全量重拉并更新存档 meta 至最新。
    self.resumeDownload = function (idOrItem, hooks) {
      const h = parseHooks(hooks);
      const item = self.findItem(idOrItem);
      if (!item) return Promise.reject(new Error('下载项不存在'));
      const filename = item.filename;
      const chunkSize = (item.meta && item.meta.chunkSize) || calcChunkSize(Number(item.total) || 0);
      const total = Math.floor(Number(item.totalSize) > 0 ? Number(item.totalSize) : Number(item.total));
      const totalChunks = (item.meta && item.meta.totalChunks) || chunkCount(Math.max(0, total), chunkSize);

      function continueResume(serverMeta) {
        // 更新存档 meta 为最新 stat（恢复后以最新 mtime/checksum 为准，供后续恢复比对）。
        const meta = Object.assign(copyMeta(item), {
          mtimeNano: serverMeta.mtime ? String(serverMeta.mtime) : '',
          checksum: serverMeta.checksum ? String(serverMeta.checksum) : '',
          chunkSize: chunkSize,
          totalChunks: totalChunks,
        });
        const force = h.force === true;
        const bm = force ? new Array(totalChunks).fill(0) : sliceToLen(item.meta && item.meta.chunksBitmap, totalChunks);
        meta.chunksBitmap = bm;
        // loaded 按 bitmap 重算（防止存档 loaded 与 bitmap 不一致，如重启后手改）。
        let l = 0;
        for (let i = 0; i < bm.length; i++) {
          if (bm[i]) l += Math.min(chunkSize, Math.max(0, total - i * chunkSize));
        }
        const upd = persistItem(item, { loaded: l, totalSize: total, total: total, meta: meta });
        const missing = force ? allChunkIndices(totalChunks) : missingChunkList(bm, totalChunks);
        return run(upd, filename, chunkSize, missing, h, force);
      }

      if (h.force) {
        // 强制重下：不 stat，按已有存档 meta 全量重拉。
        return continueResume({
          mtime: (item.meta && item.meta.mtimeNano) || '',
          checksum: (item.meta && item.meta.checksum) || '',
        });
      }
      // stat HEAD 取 size/mtime/checksum；失败回落空 meta（无法比对时不拒绝续传）。
      return statResolve(transport, filename).then(function (serverMeta) {
        const stored = {
          size: Number(item.total) || 0,
          mtime: (item.meta && item.meta.mtimeNano) || '',
          checksum: (item.meta && item.meta.checksum) || '',
        };
        if (!metaMatches(stored, serverMeta)) {
          if (h.onMismatch) h.onMismatch(serverMeta, item);
          return { mismatched: true };
        }
        return continueResume(serverMeta);
      });
    };

    function statResolve(transport, filename) {
      return Promise.resolve().then(function () {
        if (!transport || typeof transport.coreRequest !== 'function') throw new Error('传输层不可用');
        return transport.coreRequest('HEAD', '/api/files/stat?filename=' + encodeURIComponent(filename), {});
      }).then(serverMetaFromStat).catch(function () { return { size: 0, mtime: '', checksum: '' }; });
    }

    // pauseDownload(id)：中断在途 + 写回 paused（保留缓存块供恢复）。同步写 paused 后
    // abort，让在途请求尽快中断（pump 的各 fetcher 遇 abort/stopped 转为已决值不再入队新块）。
    self.pauseDownload = function (id) {
      return new Promise(function (resolve) {
        const item = self.findItem(id);
        if (!item) { resolve(); return; }
        const sess = self._running[item.id];
        if (sess) { sess.state = 'paused'; sess.aborter.abort(); delete self._running[item.id]; }
        if (item.status !== 'completed') persistItem(item, { status: 'paused' });
        resolve();
      });
    };

    // cancelDownload(id)：中断 + 清 IDB 块 + removeItem。
    self.cancelDownload = function (id) {
      return Promise.resolve().then(function () {
        const item = self.findItem(id);
        if (!item) return;
        const sess = self._running[item.id];
        if (sess) { sess.state = 'cancelled'; sess.aborter.abort(); delete self._running[item.id]; }
        if (typeof store.deleteChunkRange === 'function') {
          store.deleteChunkRange(item.id).catch(function () { /* 清缓存失败容忍 */ });
        }
        store.removeItem(item.id);
      });
    };

    // isRunning(id)：会话内是否进行中（供 UI 禁用重复操作/防止暂停后误判）。
    self.isRunning = function (id) {
      const s = self._running[id];
      return !!(s && s.state === 'running');
    };
  }

  return {
    createDownloadManager,
    // 纯函数导出（测试用）
    chunkUrlFor, calcChunkSize, chunkCount, allChunkIndices, missingChunkList,
    headerValue, serverMetaFromStat, metaMatches,
    idFor, metaFromStat,
  };
});
