/*
 * Copyright 2026 The Cocomhub Authors. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// 传输数据层（TransferItem 持久化 + IndexedDB 下载块缓存 + 上传文件句柄库）。
//
// UMD：浏览器挂 window.transferStore；Node 下 module.exports（顶部无 DOM/window/网络副作用，
// 可被 Node require 做单测——DOM/IDB/localStorage 仅函数运行期访问）。
//
// 存储约定（与 spec 一致，读 docs/superpowers/specs/2026-08-27-transfer-manager-design.md）：
//   - localStorage key `sproxy_transfer_items`：TransferItem 数组 JSON。
//   - IndexedDB 库 `sproxy-dl-cache` / 仓库 `chunks`，复合主键 [itemId, chunkIndex]，值 {itemId, chunkIndex, data:ArrayBuffer, size}。
//   - IndexedDB 库 `sproxy-up-dev` / 仓库 `fileHandles`，主键 uploadId，值 {uploadId, fileHandle}。
//   - 旧键 `sproxy_upload_sessions` 不读、不迁移（无过渡期）。
//
// 注入式：createTransferStore({ls, idb, idbKeyRange})——测试传内存 fake；浏览器默认
// window.localStorage / window.indexedDB / window.IDBKeyRange。所有 IDB 请求经
// _idbRequest promisify。createTransferStore 可被多次调用（每实例独立 IDB open 缓存）——app.js
// 一次性 create 后复用返回对象。
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.transferStore = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  const ITEMS_KEY = 'sproxy_transfer_items';
  const DL_DB = 'sproxy-dl-cache';
  const UP_DB = 'sproxy-up-dev';
  const CHUNKS_STORE = 'chunks';
  const HANDLES_STORE = 'fileHandles';

  // ---- 纯函数：归一化 ----------------

  // normalizeItems 把任意反序列化输入（localStorage 值）归一为 TransferItem 数组。
  // 非数组 / null → []；数组则筛掉明显非法条目（缺 id 或 id 非字符串）。
  function normalizeItems(raw) {
    if (!Array.isArray(raw)) return [];
    const out = [];
    for (const it of raw) {
      if (it && typeof it === 'object' && typeof it.id === 'string' && it.id) out.push(it);
    }
    return out;
  }

  // computeChunkIndex 由文件 offset 计算块序号（chunkSize 制）。非法/负值归 0；
  // chunkSize 非正 → 0；字符串数字（如 '8'）经 Number 可转。
  function computeChunkIndex(offset, chunkSize) {
    const o = Number(offset);
    const c = Number(chunkSize);
    if (!isFinite(o) || o < 0) return 0;
    if (!isFinite(c) || c <= 0) return 0;
    return Math.floor(o / c);
  }

  // chunkCountOf 由总字节数向上取整得分块数。非法输入返回 0。
  function chunkCountOf(totalBytes, chunkSize) {
    const n = Number(totalBytes);
    const c = Number(chunkSize);
    if (!isFinite(n) || n <= 0) return 0;
    if (!isFinite(c) || c <= 0) return 0;
    return Math.ceil(n / c);
  }

  // ---- localStorage 会话层（容错） ----

  function loadItems(ls) {
    try {
      if (!ls) return [];
      const raw = ls.getItem(ITEMS_KEY);
      if (raw == null) return [];
      return normalizeItems(JSON.parse(raw));
    } catch (e) {
      return [];
    }
  }

  function saveItems(ls, items) {
    if (!ls) return;
    try {
      ls.setItem(ITEMS_KEY, JSON.stringify(normalizeItems(items)));
    } catch (e) { /* 配额满/禁用时不抛——传输页不因持久化失败崩溃 */ }
  }

  function upsertItem(ls, item) {
    if (!item || typeof item.id !== 'string' || !item.id) return;
    const items = loadItems(ls);
    const idx = items.findIndex(function (it) { return it.id === item.id; });
    if (idx >= 0) items[idx] = item; else items.push(item);
    saveItems(ls, items);
  }

  function removeItem(ls, id) {
    if (!id) return;
    saveItems(ls, loadItems(ls).filter(function (it) { return it.id !== id; }));
  }

  // ---- IndexedDB 封装（promisify） ----

  // _idbRequest 把 IDBRequest/IDBOpenDBRequest 事件改 Promise。
  // 成功 → resolve(req.result)；失败 → reject(req.error)。upgrade 事件在 openDB 中单独挂。
  function _idbRequest(req) {
    return new Promise(function (resolve, reject) {
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { const e = req.error || new Error('IndexedDB error'); reject(e); };
      req.onblocked = function () { /* 升级被占用阻塞——忽略（窗口期极短） */ };
    });
  }

  // openDB 打开（必要时创建）数据库并跑 upgrade 回调建仓库。返回 Promise<IDBDatabase>。
  function openDB(idb, name, version, upgrade) {
    const req = idb.open(name, version);
    req.onupgradeneeded = function (ev) {
      const db = ev.target.result;
      if (upgrade) upgrade(db);
    };
    return _idbRequest(req);
  }

  // ---- 公开 API ----

  function createTransferStore(opts) {
    opts = opts || {};
    const ls = opts.ls !== undefined ? opts.ls
      : (typeof window !== 'undefined' && window.localStorage) ? window.localStorage : null;
    const idb = opts.idb !== undefined ? opts.idb
      : (typeof window !== 'undefined' && window.indexedDB) ? window.indexedDB : null;
    const Range = opts.idbKeyRange !== undefined ? opts.idbKeyRange
      : (typeof window !== 'undefined' && window.IDBKeyRange) ? window.IDBKeyRange : null;

    // 会话 API（localStorage 封装，容错）
    function load() { return loadItems(ls); }
    function save(items) { return saveItems(ls, items); }
    function upsert(item) { return upsertItem(ls, item); }
    function remove(id) { return removeItem(ls, id); }

    // 双库分别打开（spec 字面值）：sproxy-dl-cache（块缓存）与 sproxy-up-dev（上传文件句柄）。
    // 各自独立 lazily open 并缓存 Promise；upgrade 事件只建本库所属仓库。
    let dlPromise = null;
    function getOpenDB() {
      if (idb == null) return Promise.reject(new Error('IndexedDB unavailable in this environment'));
      if (!dlPromise) {
        dlPromise = openDB(idb, DL_DB, 1, function (db) {
          if (!db.objectStoreNames.contains(CHUNKS_STORE)) {
            db.createObjectStore(CHUNKS_STORE, { keyPath: ['itemId', 'chunkIndex'] });
          }
        });
      }
      return dlPromise;
    }
    let upPromise = null;
    function getUpDB() {
      if (idb == null) return Promise.reject(new Error('IndexedDB unavailable in this environment'));
      if (!upPromise) {
        upPromise = openDB(idb, UP_DB, 1, function (db) {
          if (!db.objectStoreNames.contains(HANDLES_STORE)) {
            db.createObjectStore(HANDLES_STORE, { keyPath: 'uploadId' });
          }
        });
      }
      return upPromise;
    }

    // 写确认策略（Important 审查项）——本实现采用 b) 请求成功即确认 + 注释权衡：
    // 写请求（put/delete）的 success 在真实浏览器中仅当该请求成功且已（或即将）提交
    // 到本次事务时才触发；随后浏览器自动提交事务，unload 或忽略的窗口极短。数据层只
    // 负责把写请求排入事务；若要求『落盘后才让上层继续』的严格持久性保证，需双事件
    // 等待（tx.oncomplete + 请求失败恰好抛错），但本库写请求成功即返回的宽容语义与
    // 断点续传的『尽力而为落盘 + 重启后按服务端会话对账』模型一致（重启恢复以服务端
    // 会话为权威，本地块缓存只是加速缓冲，丢失可重写）。unload 时机可接受（极小
    // 窗口 + 可恢复），故不走 a)。
    // saveChunk(itemId, chunkIndex, data:ArrayBuffer, size) — 复合主键 upsert。
    function saveChunk(itemId, chunkIndex, data, size) {
      return getOpenDB().then(function (db) {
        return _idbRequest(db.transaction(CHUNKS_STORE, 'readwrite').objectStore(CHUNKS_STORE)
          .put({ itemId: itemId, chunkIndex: chunkIndex, data: data, size: size }));
      });
    }

    // listChunkCount(itemId) — 该 item 已缓存块数（跨 item 隔离：range lower [id, 0]、
    // upper [id, Infinity) 半开区间）。无 IDBKeyRange 时退回枚举 getAllKeys 计数。
    function listChunkCount(itemId) {
      return getOpenDB().then(function (db) {
        const store = db.transaction(CHUNKS_STORE, 'readonly').objectStore(CHUNKS_STORE);
        if (Range) {
          return _idbRequest(store.count(Range.bound([itemId, 0], [itemId, Infinity], false, true)));
        }
        return _idbRequest(store.getAllKeys()).then(function (keys) {
          let n = 0;
          for (const k of keys || []) {
            if (Array.isArray(k) && k[0] === itemId) n++;
          }
          return n;
        });
      });
    }

    // loadChunk(itemId, chunkIndex) — 单块读取；缺 → null。记录结构含 {data,size}。
    function loadChunk(itemId, chunkIndex) {
      return getOpenDB().then(function (db) {
        return _idbRequest(db.transaction(CHUNKS_STORE, 'readonly').objectStore(CHUNKS_STORE).get([itemId, chunkIndex]));
      }).then(function (rec) { return rec || null; });
    }

    // deleteChunkRange(itemId) — 删除该 item 全部已缓存块（完成合并/取消时清）。
    // 范围同上 [itemId, 0]..[itemId, ∞) 半开。无 IDBKeyRange → reject（调用方按失败处理）。
    function deleteChunkRange(itemId) {
      if (!Range) return Promise.reject(new Error('IDBKeyRange unavailable'));
      return getOpenDB().then(function (db) {
        return _idbRequest(db.transaction(CHUNKS_STORE, 'readwrite').objectStore(CHUNKS_STORE)
          .delete(Range.bound([itemId, 0], [itemId, Infinity], false, true)));
      });
    }

    // saveFileHandle(uploadId, fileHandle) / getFileHandle(uploadId) — 上传续传用。
    // 操作独立库 sproxy-up-dev / fileHandles（主键 uploadId）。
    // 仅 Chromium-class 浏览器的 File System Access API 可用时才有实际句柄；
    // 句柄不可得 → 回落『重选文件』路径（调用应用层处理）。
    function saveFileHandle(uploadId, fileHandle) {
      return getUpDB().then(function (db) {
        return _idbRequest(db.transaction(HANDLES_STORE, 'readwrite').objectStore(HANDLES_STORE)
          .put({ uploadId: uploadId, fileHandle: fileHandle }));
      });
    }

    function getFileHandle(uploadId) {
      return getUpDB().then(function (db) {
        return _idbRequest(db.transaction(HANDLES_STORE, 'readonly').objectStore(HANDLES_STORE).get(uploadId));
      }).then(function (rec) { return (rec && rec.fileHandle) || null; });
    }

    // queryFileHandlePermission(fileHandle, mode='read') → Promise<null|'granted'|'prompt'|'denied'>
    // 无句柄或句柄不支持查询权限 → null（调用方回落『重选文件』路径）。
    // queryPermission 抛错/拒绝 → null（不向外抛，保持数据层容错气质）。
    function queryFileHandlePermission(fileHandle, mode) {
      mode = mode || 'read';
      if (!fileHandle || typeof fileHandle.queryPermission !== 'function') {
        return Promise.resolve(null);
      }
      return Promise.resolve().then(function () {
        return fileHandle.queryPermission({ mode: mode });
      }).catch(function () { return null; });
    }

    return {
      // 纯函数
      normalizeItems: normalizeItems,
      computeChunkIndex: computeChunkIndex,
      chunkCountOf: chunkCountOf,
      // localStorage 层
      loadItems: load,
      saveItems: save,
      upsertItem: upsert,
      removeItem: remove,
      // IDB 封装
      getOpenDB: getOpenDB,
      getUpDB: getUpDB,
      saveChunk: saveChunk,
      listChunkCount: listChunkCount,
      loadChunk: loadChunk,
      deleteChunkRange: deleteChunkRange,
      saveFileHandle: saveFileHandle,
      getFileHandle: getFileHandle,
      queryFileHandlePermission: queryFileHandlePermission,
    };
  }

  return {
    createTransferStore: createTransferStore,
    normalizeItems: normalizeItems,
    computeChunkIndex: computeChunkIndex,
    chunkCountOf: chunkCountOf,
  };
});
