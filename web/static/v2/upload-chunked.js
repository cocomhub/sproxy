// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 分块上传（P1d）：与旧版 sclient 协议一致（init → status → chunk* → complete）。
// 纯函数（阈值/分块索引）可 Node 单测；网络部分经注入的 api/util/crypto 完成。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.v2chunked = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  var BASE = 4 * 1024 * 1024;
  var MAX = 64 * 1024 * 1024;
  var SIMPLE_LIMIT = 8 * 1024 * 1024;

  /** 与服务端协商前的初始分块大小（init 返回的 chunk_size 为准）。 */
  function calcChunkSize(size) {
    var c = BASE;
    while (c * 512 < size && c < MAX) { c *= 2; }
    return Math.min(c, MAX);
  }

  /** 缺失分块优先；否则全量索引。 */
  function buildChunkIndices(totalChunks, missing) {
    if (Array.isArray(missing)) { return missing.slice(); }
    var out = [];
    for (var i = 0; i < totalChunks; i += 1) { out.push(i); }
    return out;
  }

  /** 上传会话 id：filename|size|mtimeNano|checksum 的 sha256 前 32 hex（对齐旧版）。 */
  function uploadIdFor(filename, totalSize, mtimeNano, checksum, sha256Hex) {
    return sha256Hex(filename + '|' + totalSize + '|' + mtimeNano + '|' + checksum)
      .then(function (hex) { return String(hex).substring(0, 32); });
  }

  /** 大文件增量 sha256（8MiB 以下一次读；以上按 64MiB 切片，避免整文件驻留内存）。 */
  function fileSha256(file, shaCtor, sha256Hex, onProgress) {
    if (file.size <= SIMPLE_LIMIT) {
      return file.arrayBuffer().then(function (buf) {
        var bytes = new Uint8Array(buf);
        if (onProgress) { onProgress(file.size, file.size); }
        return sha256Hex(bytes);
      });
    }
    var sha = new shaCtor();
    var cs = 64 * 1024 * 1024;
    var total = file.size;
    var acc = 0;
    var chain = Promise.resolve();
    var index = 0;
    var count = Math.ceil(total / cs);
    var step = function () {
      if (index >= count) { return Promise.resolve(sha.digest()); }
      var start = index * cs;
      var end = Math.min(start + cs, total);
      index += 1;
      return file.slice(start, end).arrayBuffer().then(function (buf) {
        sha.update(new Uint8Array(buf));
        acc += end - start;
        if (onProgress) { onProgress(acc, total); }
        return step();
      });
    };
    return step();
  }

  /**
   * 执行分块上传。
   * @param {Blob} file
   * @param {object} deps {api, util, cryptoLib, shaCtor, join}
   * @param {object} opts {subdir, onProgress(loaded,total), onSession(id), isCancelled()}
   */
  function uploadChunked(file, deps, opts) {
    var o = opts || {};
    var rel = deps.join(o.subdir || '', file.name);
    var mtimeNano = (file.lastModified || Date.now()) * 1e6;
    var chunkHint = calcChunkSize(file.size);
    var total = file.size;
    var checksum = '';
    var sessionId = '';

    return fileSha256(file, deps.shaCtor, deps.cryptoLib.sha256Hex, o.onHashProgress)
      .then(function (sum) {
        checksum = sum;
        return uploadIdFor(rel, total, mtimeNano, checksum, deps.cryptoLib.sha256Hex);
      })
      .then(function (uploadId) {
        return deps.api.json('POST', '/upload/init', {
          body: new TextEncoder().encode(JSON.stringify({
            upload_id: uploadId,
            filename: rel,
            total_size: total,
            chunk_size: chunkHint,
            total_chunks: Math.ceil(total / chunkHint),
            file_checksum: checksum,
            file_mod_time: mtimeNano
          })),
          contentType: 'application/json'
        }).then(function (init) {
          if (!init.success) { throw new Error(init.message || '初始化失败'); }
          if (init.upload_id === 'already_exists') { return { skipped: true }; }
          sessionId = init.upload_id;
          if (o.onSession) { o.onSession(sessionId); }
          return deps.api.json('GET', '/upload/status?upload_id=' + encodeURIComponent(sessionId))
            .catch(function () { return {}; })
            .then(function (st) {
              var serverChunk = init.chunk_size || chunkHint;
              var totalChunks = Math.ceil(total / serverChunk);
              var indices = buildChunkIndices(totalChunks, st && st.success ? st.missing_chunks : null);
              var loaded = 0;
              var seq = Promise.resolve();
              indices.forEach(function (idx) {
                seq = seq.then(function () {
                  if (o.isCancelled && o.isCancelled()) {
                    var err = new Error('上传已暂停');
                    err.code = 'E_CANCELLED';
                    throw err;
                  }
                  var start = idx * serverChunk;
                  var end = Math.min(start + serverChunk, total);
                  return file.slice(start, end).arrayBuffer().then(function (buf) {
                    var bytes = new Uint8Array(buf);
                    return deps.cryptoLib.sha256Hex(bytes).then(function (chunkSum) {
                      var mp = deps.util.buildMultipart(
                        { upload_id: sessionId, chunk_index: String(idx), chunk_checksum: chunkSum },
                        { name: 'chunk', filename: String(idx).padStart(5, '0') + '.chunk', contentType: 'application/octet-stream', bytes: bytes }
                      );
                      return deps.api.request('POST', '/upload/chunk', { body: mp.body, contentType: mp.contentType })
                        .then(deps.api.ensureOk)
                        .then(function (resp) { return resp.json(); })
                        .then(function (r) {
                          if (!r.success && !r.should_retry) { throw new Error(r.message || ('分块 ' + idx + ' 上传失败')); }
                          loaded += end - start;
                          if (o.onProgress) { o.onProgress(loaded, total); }
                        });
                    });
                  });
                });
              });
              return seq.then(function () {
                return deps.api.json('POST', '/upload/complete', {
                  body: new TextEncoder().encode(JSON.stringify({ upload_id: sessionId })),
                  contentType: 'application/json'
                }).then(function (done) {
                  if (!done.success) { throw new Error(done.message || '合并失败'); }
                  return { skipped: false, uploadId: sessionId, checksum: done.file_checksum || checksum };
                });
              });
            });
        });
      });
  }

  return {
    SIMPLE_LIMIT: SIMPLE_LIMIT,
    calcChunkSize: calcChunkSize,
    buildChunkIndices: buildChunkIndices,
    uploadIdFor: uploadIdFor,
    uploadChunked: uploadChunked
  };
});
