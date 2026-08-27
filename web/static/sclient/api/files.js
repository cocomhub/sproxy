/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * files.js —— sclient 领域 API：文件 CRUD / 目录 / 批量 / 归档 / 版本 / 上传。
 *
 * 闭包式工厂：api/files.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 归一：列表类返回 JSON 对象（coreRequest 的 body 经 util.decodeJSON 解析）；
 * 操作类返回 {success, message, ...}；download/archive 返回 {blob, headers}（headers
 * 保留 X-File-Checksum 等响应头供 UI 完整性校验；archive 额外给 {filename, ...}）。
 *
 * 端点语义对齐 server handlers（handlers.go / list_handler.go / chunked_upload.go）：
 *   - list(subdir, opts)        GET  /api/files?subdir=&offset=&limit=&sort=&order=
 *   - search(q)                 GET  /api/files/search?q=（服务端只消费 q；subdir/offset/limit 不消费）
 *   - stat(filename)           HEAD /api/files/stat?filename=（返回{status,headers,body}）
 *   - download(filename, opts)  GET  /download?filename=（opts.headers 透传含 Range）
 *   - deleteFile(name, chk)    POST /delete?filename=（X-File-Checksum 头）
 *   - rename(from,to,chk)      POST /rename?from=&to=（X-File-Checksum 头）
 *   - mkdir/rmdir(dir)         POST /mkdir?dirname= / POST /rmdir?dirname=&force=true
 *   - batchDelete(files)        POST /api/batch/delete  files=[{filename,checksum}]
 *   - batchRename(ops)         POST /api/batch/rename   ops=[{from,to,checksum}]
 *   - archive(files)           POST /api/archive（下载归档 → {success,blob,filename}）
 *   - archiveDir(dir)          GET  /api/archive-dir?dirname=（→ 同上）
 *   - versions.list/restore/delete
 *   - upload(file, {subdir, onProgress, forceChunked}) → {success,message,checksum?,...}
 *
 * upload：小文件（≤cfg.chunkThreshold，默认 8 MiB）走简单 POST /upload（先算
 * SHA-256，buildMultipart 字节由 coreRequest 自动签名）；大文件/mesh 走分块
 * init→chunk（每块独立签名）→complete。onProgress 回调由 UI 传：简单上传传字节
 * (loaded,total)；分块上传段传对象 {loaded, total, chunkIndex, totalChunks} 供 UI 渲染
 * 「done/total 分块」进度文案。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiFiles = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  const TE = new TextEncoder();
  const TD = new TextDecoder();

  const BASE_CHUNK_SIZE = 4 * 1024 * 1024;
  const MAX_CHUNK_SIZE = 64 * 1024 * 1024;

  // ---- 纯函数区（无 ctx，可在模块级导出供测试）----

  // 分块大小协商（对齐 upload.js calcChunkSize 与服务端上限）。
  function calcChunkSize(fileSize) {
    let chunkSize = BASE_CHUNK_SIZE;
    while (chunkSize * 512 < fileSize && chunkSize < MAX_CHUNK_SIZE) {
      chunkSize *= 2;
    }
    return Math.min(chunkSize, MAX_CHUNK_SIZE);
  }

  // 上传会话 ID：filename|size|mtimeNano|checksum 的 SHA-256 前 32 位 hex。
  // 对齐 upload.js generateUploadId（WebCrypto subtle.digest）。
  async function generateUploadId(filename, totalSize, mtimeNano, checksum) {
    const raw = filename + '|' + totalSize + '|' + mtimeNano + '|' + checksum;
    const digest = await crypto.subtle.digest('SHA-256', TE.encode(raw));
    return hexBytes(new Uint8Array(digest)).substring(0, 32);
  }

  function hexBytes(bytes) {
    let out = '';
    for (let i = 0; i < bytes.length; i++) out += bytes[i].toString(16).padStart(2, '0');
    return out;
  }

  function sha256Bytes(bytes) {
    return crypto.subtle.digest('SHA-256', bytes).then(function (d) { return hexBytes(new Uint8Array(d)); });
  }

  // 文件/Blob → SHA-256 hex。小文件（≤8 MiB）单次 arrayBuffer；大文件分片增量
  //（sclient/sha256.js 的 Sha256.update 接受 Uint8Array，digest() 返回 hex）——每片最大
  // 64MiB、随读随弃，绝不整文件物化（历史曾所有分片拼接成单个巨型 ArrayBuffer，
  // 大文件触发 Array buffer allocation failed：RangeError）。onChunk(accumulated,total)
  // 每读一块回调（可选，用于进度）。
  async function computeSHA256(blob, onChunk) {
    const total = blob.size || 0;
    const readWhole = total <= 8 * 1024 * 1024;
    if (readWhole) {
      const bytes = new Uint8Array(await blob.arrayBuffer());
      if (onChunk) onChunk(total, total);
      return await sha256Bytes(bytes);
    }
    const cs = Math.min(64 * 1024 * 1024, total);
    const n = Math.ceil(total / cs);
    const sha = new sclientSha256();
    let acc = 0;
    for (let i = 0; i < n; i++) {
      const s = i * cs;
      const e = Math.min(s + cs, total);
      const buf = new Uint8Array(await blob.slice(s, e).arrayBuffer());
      sha.update(buf);
      acc += buf.byteLength;
      if (onChunk) onChunk(acc, total);
    }
    return sha.digest();
  }

  function encodeJSON(obj) { return TE.encode(JSON.stringify(obj)); }

  function blobFromBytes(bytes, type) {
    if (typeof Blob !== 'undefined' && (bytes instanceof Blob)) return bytes;
    return new Blob([bytes], { type: type || 'application/octet-stream' });
  }

  function firstStringHeader(headers, key) {
    if (!headers || !key) return '';
    const direct = headers[key] || headers[key.toLowerCase()] || headers[key.toUpperCase()];
    if (Array.isArray(direct)) return direct[0] || '';
    if (typeof direct === 'string') return direct;
    if (direct !== undefined && direct !== null) return String(direct);
    return '';
  }

  // 从响应 headers 中提取指定 key 的字符串映射（未注入任何 key 时返回 {}）。
  // 兼容两种形态：flat（隧道 metadata headers 已由 transport 展开为 {name:value}）
  // 与 Headers 对象（直连 fetch）。下载返回结构中的 headers 由此构造。
  function extractHeaderMap(headers, wanted) {
    const keys = wanted && Object.keys(wanted);
    if (!keys || keys.length === 0) return {};
    const out = {};
    for (const k of keys) {
      const v = firstStringHeader(headers, k);
      if (v) out[k] = v;
    }
    return out;
  }

  // ---- 领域方法工厂 ----
  return function createFilesApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/files: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/files: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;
    const config = ctx.config;
    const log = ctx.log;

    // 通用 JSON 请求：method/path → coreRequest → decodeJSON 合并进结果。
    // opts.headers / opts.bodyBytes 可覆盖默认（分块上传 multipart 用）。
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

    function decodeOp(res) {
      const d = util.decodeJSON(res.body);
      return Object.assign({ success: !!d.success, message: d.message || '' }, d, { status: res.status });
    }

    // ---- 文件列表 / 搜索 / stat ----
    function list(subdir, opts) {
      const p = opts || {};
      const path = urlWithParams('/api/files', {
        subdir: subdir || '',
        offset: p.offset !== undefined ? String(p.offset) : '0',
        limit: p.limit !== undefined ? String(p.limit) : '500',
        sort: p.sort,
        order: p.order,
      });
      return jsonRequest('GET', path).then(function (d) { return d; });
    }

    // search 仅消费 q：服务端 searchFiles 只读 URL 的 q 参数（subdir/offset/limit 均无
    // 语义）。保持 search(q) 单参签名，杜绝死透传（历史曾把 subdir 透传给服务端，不消费）。
    function search(q) {
      return jsonRequest('GET', urlWithParams('/api/files/search', { q: q }));
    }

    function stat(filename) {
      return coreRequest('HEAD', '/api/files/stat?filename=' + encodeURIComponent(filename), {});
    }

    // ---- 下载：返回 { blob, headers }（隧道 mode 流式、direct arrayBuffer→Blob）。
    // headers 保留 X-File-Checksum 以便 UI 做本地 SHA-256 往返校验（C-1 遗留：旧版
    // 只回 Blob 不回响应头，导致直连模式 UI 无法校验）。assert: blob 字节与 header
    // 一致性由调用方（app.js downloadFile）负责——此处只透传。
    function download(filename, opts) {
      const p = opts || {};
      const headers = Object.assign({}, p.headers || {});
      const dlHeaders = Object.assign({}, p.downloadHeaders || {});
      return coreRequest('GET', '/download?filename=' + encodeURIComponent(filename), {
        headers: headers,
        download: true,
        // 注：collectHeaders 不透传给 transport（transport 不消费该字段）——
        // 响应头过滤只走下方 extractHeaderMap(res.headers, dlHeaders)，勿再传 collectHeaders。
      }).then(function (res) {
        return { blob: blobFromBytes(res.body, p.type || 'application/octet-stream'), headers: extractHeaderMap(res.headers, dlHeaders) };
      });
    }

    // ---- 目录 ----
    function mkdir(dirname) {
      return jsonRequest('POST', '/mkdir?dirname=' + encodeURIComponent(dirname), undefined);
    }
    function rmdir(dirname) {
      // 服务端对非空目录要求 ?force=true（防误删）；Web 删除目录前已 confirm，
      // 故此处永远带 force=true（手动删目录同样过 confirm）。
      return jsonRequest('POST', '/rmdir?dirname=' + encodeURIComponent(dirname) + '&force=true', undefined);
    }

    // ---- 删除 / 重命名（需要 X-File-Checksum 头）----
    function deleteFile(filename, checksum) {
      const path = '/delete?filename=' + encodeURIComponent(filename);
      return coreRequest('POST', path, { headers: { 'X-File-Checksum': checksum || '' } }).then(decodeOp);
    }
    function rename(from, to, checksum) {
      const path = '/rename?from=' + encodeURIComponent(from) + '&to=' + encodeURIComponent(to);
      return coreRequest('POST', path, { headers: { 'X-File-Checksum': checksum || '' } }).then(decodeOp);
    }

    // ---- 批量 ----
    function batchDelete(files) { return jsonRequest('POST', '/api/batch/delete', { files: files }); }
    function batchRename(operations) { return jsonRequest('POST', '/api/batch/rename', { operations: operations }); }

    // ---- 归档（下载型，返回 {success, blob, filename}）----
    function archive(files) {
      return coreRequest('POST', '/api/archive', {
        headers: { 'Content-Type': 'application/json' },
        bodyBytes: encodeJSON({ files: files }),
        download: true,
      }).then(function (res) { return blobArchive(res, 'archive.tar.gz'); });
    }
    function archiveDir(dirname) {
      return coreRequest('GET', '/api/archive-dir?dirname=' + encodeURIComponent(dirname), { download: true })
        .then(function (res) { return blobArchive(res, null); });
    }

    function blobArchive(res, defaultName) {
      let name = defaultName || 'archive.tar.gz';
      const cd = firstStringHeader(res.headers, 'Content-Disposition');
      if (cd) {
        const m = cd.match(/filename="?([^";\s]+)"?/);
        if (m && m[1]) name = m[1];
      }
      return { success: true, blob: blobFromBytes(res.body, 'application/gzip'), filename: name };
    }

    // ---- 版本 ----
    const versions = {
      list: function (filename) {
        return jsonRequest('GET', '/api/versions?filename=' + encodeURIComponent(filename), undefined);
      },
      restore: function (filename, versionId) {
        return jsonRequest('POST', '/api/versions/restore?filename=' + encodeURIComponent(filename) + '&version_id=' + encodeURIComponent(String(versionId)), {});
      },
      delete: function (filename, versionId) {
        return jsonRequest('DELETE', '/api/versions?filename=' + encodeURIComponent(filename) + '&version_id=' + encodeURIComponent(String(versionId)), {});
      },
    };

    // ---- 上传（简单 / 分块）----
    async function upload(file, opts) {
      const p = opts || {};
      if (!file || (typeof file.arrayBuffer !== 'function')) throw new TypeError('api/files.upload 需要 Blob/File 对象');
      const fileName = (p.subdir ? p.subdir + '/' : '') + (file.name || 'upload');
      const threshold = config && config.chunkThreshold ? config.chunkThreshold : 8 * 1024 * 1024;
      const forceChunked = p.forceChunked === true;
      const useChunked = forceChunked || file.size > threshold;
      if (log) log.debug('upload 决策', { fileName: fileName, size: file.size, useChunked: useChunked });
      if (!useChunked) return simpleUpload(file, fileName, p);
      return chunkedUpload(file, fileName, p);
    }

    // ---- 简单上传 ----
    async function simpleUpload(file, fileName, p) {
      const checksum = await computeSHA256(file, p.onProgress);
      const bytes = new Uint8Array(await file.arrayBuffer());
      const mp = util.buildMultipart({}, {
        name: 'file', filename: fileName, contentType: 'application/octet-stream', bytes: bytes,
      });
      const res = await coreRequest('POST', '/upload', {
        headers: {
          'Content-Type': mp.contentType,
          'X-File-Checksum': checksum,
          'X-File-Path': fileName,
          'X-File-MTime': String(((file.lastModified) || Date.now()) * 1000000),
        },
        bodyBytes: mp.body,
      });
      const d = util.decodeJSON(res.body);
      return Object.assign({ success: !!d.success, message: d.message || '', checksum: d.file_checksum || checksum, filename: fileName }, d, { status: res.status });
    }

    // ---- 分块上传 ----
    async function chunkedUpload(file, fileName, p) {
      const totalSize = file.size;
      const chunkSize = calcChunkSize(totalSize);
      const persist = typeof p.onSession === 'function' ? p.onSession : null; // 会话持久化钩子（UI 断点续传）
      const startMtimeNano = ((file.lastModified) || Date.now()) * 1000000;

      // 计算 SHA-256 之前就先落一个基础会话（status='hashing'）——否则计算阶段（大文件
      // 耗时可达数秒~数十秒）刷新页面，localStorage 里还没有任何会话，checkResumableUploads
      // 无从发现，且此时服务端也还没有 init 会话（无法续传）。刷新后重选文件重算 checksum，
      // upload_id（seed=filename|size|mtime|checksum）不变即可续传。
      const preUploadId = await generateUploadId(fileName, totalSize, startMtimeNano, '');
      if (persist) persist({ upload_id: preUploadId, filename: fileName, totalSize: totalSize, totalChunks: Math.ceil(totalSize / chunkSize), status: 'hashing', mtimeNano: startMtimeNano });

      const checksum = await computeSHA256(file, p.onProgress);
      const mtimeNano = ((file.lastModified) || Date.now()) * 1000000;
      const uploadId = await generateUploadId(fileName, totalSize, mtimeNano, checksum);

      const initRes = await jsonRequest('POST', '/upload/init', {
        upload_id: uploadId, filename: fileName, total_size: totalSize,
        chunk_size: chunkSize, total_chunks: Math.ceil(totalSize / chunkSize),
        file_checksum: checksum, file_mod_time: mtimeNano,
      });
      if (!initRes.success) {
        return { success: false, message: initRes.message || '初始化失败', filename: fileName };
      }
      if (initRes.upload_id === 'already_exists') {
        if (persist) persist({ upload_id: 'already_exists', filename: fileName }, true);
        return { success: true, message: '文件已存在，跳过', upload_id: 'already_exists', filename: fileName };
      }
      const sessionId = initRes.upload_id;
      // 首个会话就位后立即持久化（页面刷新后 checkResumableUploads 能读到 uploading）。
      // totalChunks 以 init 返回的 chunk_size 校准。
      // 若 sessionId === preUploadId（filename/size/mtime 同源）：先移除 hashing 占位再落
      // uploading——避免同 key 残留 hashing 幽灵会话（complete 只能按 sessionId 清）。
      if (persist) {
        const serverChunkSize = initRes.chunk_size || chunkSize;
        if (sessionId === preUploadId) persist({ upload_id: preUploadId, filename: fileName }, true); // 清 hashing 占位（remove 语义）
        persist({ upload_id: sessionId, filename: fileName, totalSize: totalSize, totalChunks: Math.ceil(totalSize / serverChunkSize), fileChecksum: checksum, status: 'uploading' });
      }

      // 查询缺失分块（服务端权威列表；失败回退全量上传）。
      let missing = null;
      try {
        const st = await jsonRequest('GET', '/upload/status?upload_id=' + encodeURIComponent(sessionId), undefined);
        if (st.success) missing = st.missing_chunks || [];
      } catch (e) { /* 查询失败按全量上传处理 */ }

      const serverChunkSize = initRes.chunk_size || chunkSize;
      const totalChunksAdj = Math.ceil(totalSize / serverChunkSize);
      const indices = buildChunkIndices(totalChunksAdj, missing);

      let loaded = 0;
      for (let i = 0; i < indices.length; i++) {
        const idx = indices[i];
        const start = idx * serverChunkSize;
        const end = Math.min(start + serverChunkSize, totalSize);
        const chunkBytes = new Uint8Array(await file.slice(start, end).arrayBuffer());
        const chunkChecksum = await sha256Bytes(chunkBytes);
        const mp = util.buildMultipart(
          { upload_id: sessionId, chunk_index: String(idx), chunk_checksum: chunkChecksum },
          { name: 'chunk', filename: String(idx).padStart(5, '0') + '.chunk', contentType: 'application/octet-stream', bytes: chunkBytes }
        );
        const chunkRes = await jsonRequest('POST', '/upload/chunk', null, {
          headers: { 'Content-Type': mp.contentType },
          bodyBytes: mp.body,
        });
        if (chunkRes.success) {
          loaded += (end - start);
          if (p.onProgress) p.onProgress({ loaded: loaded, total: totalSize, chunkIndex: idx, totalChunks: totalChunksAdj });
          // 每个分块成功即更新持久化会话（进度字段，供续传 UI 展示）。
          if (persist) persist({ upload_id: sessionId, filename: fileName, totalSize: totalSize, totalChunks: totalChunksAdj, fileChecksum: checksum, status: 'uploading', completedChunks: indices.slice(0, i + 1), loaded: loaded });
        } else if (!chunkRes.should_retry) {
          return { success: false, message: chunkRes.message || ('分块 ' + idx + ' 上传失败'), upload_id: sessionId, filename: fileName };
        }
        // should_retry=true 时静默继续后续块；最终 complete 校验兜底。
      }

      const completeRes = await jsonRequest('POST', '/upload/complete', { upload_id: sessionId });
      if (!completeRes.success) {
        return { success: false, message: completeRes.message || '合并失败', upload_id: sessionId, filename: fileName };
      }
      // 合并成功/『已存在』后移除持久化会话。
      if (persist) persist({ upload_id: sessionId, filename: fileName }, true);
      return {
        success: true, filename: completeRes.filename || fileName,
        checksum: completeRes.file_checksum || checksum, upload_id: sessionId,
        message: completeRes.message || '上传成功',
      };
    }

    function buildChunkIndices(totalChunks, missingChunks) {
      if (Array.isArray(missingChunks)) return missingChunks.slice();
      const indices = [];
      for (let i = 0; i < totalChunks; i++) indices.push(i);
      return indices;
    }

    function urlWithParams(base, params) {
      const parts = [];
      for (const k of Object.keys(params)) {
        if (params[k] === undefined || params[k] === null || params[k] === '') continue;
        parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(params[k])));
      }
      return parts.length ? base + '?' + parts.join('&') : base;
    }

    return {
      list,
      search,
      stat,
      download,
      upload,
      mkdir,
      rmdir,
      deleteFile,
      rename,
      batchDelete,
      batchRename,
      archive,
      archiveDir,
      versions,
      // 模块级纯函数导出（测试用；非公共 API）
      _internals: { calcChunkSize, computeSHA256, buildChunkIndices, generateUploadId },
    };
  };
});
