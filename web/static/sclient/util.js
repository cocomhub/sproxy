/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * util.js —— sclient 前端库的纯函数工具（跨领域方法 / 测试共享）。
 *
 * 职责：
 *   - decodeJSON(u8|string)  —— 传输层返回的 body(字节) → JS 对象（对齐
 *     app.js「JSON.parse(new TextDecoder().decode(result.body))」语义；字符串直传）。
 *   - encodeJSON(obj)        —— JS 对象 → UTF-8 Uint8Array（供 coreRequest 签名+发送）。
 *   - buildMultipart(fields, fileField) —— 从 upload.js buildMultipartBody 搬入的
 *     纯 multipart 组装（无 DOM 依赖），返回 {body: Uint8Array, contentType: string}。
 *   - concatBytes(...)       —— 字节拼接辅助。
 *
 * 浏览器暴露全局 sclientUtil；Node 中 module.exports 导出（供单测使用）。
 * 本文件为纯函数，不依赖 crypto/config/log/transport 任何模块。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.sclientUtil = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  const TE = new TextEncoder();
  const TD = new TextDecoder();

  // 把 coreRequest 返回的 body 解码为 JS 对象。
  // null/undefined → {}；string 原样 JSON.parse；Uint8Array 先 UTF-8 解码再 parse。
  // 解析失败抛 SyntaxError（由调用方归一为 SclientError / 直接向上抛）。
  function decodeJSON(body) {
    if (body == null) return {};
    let text;
    if (typeof body === 'string') {
      text = body;
    } else if (body.byteLength !== undefined) {
      text = TD.decode(body);
    } else {
      text = String(body);
    }
    if (text === '') return {};
    return JSON.parse(text);
  }

  // 把 JS 对象编码为 UTF-8 字节（用于 coreRequest bodyBytes + SproxySig 预哈希）。
  function encodeJSON(obj) {
    return TE.encode(JSON.stringify(obj));
  }

  // 字节拼接（接受 Uint8Array / null）。
  function concatBytes() {
    let total = 0;
    for (let i = 0; i < arguments.length; i++) {
      if (arguments[i]) total += arguments[i].length;
    }
    const out = new Uint8Array(total);
    let off = 0;
    for (let i = 0; i < arguments.length; i++) {
      const a = arguments[i];
      if (a) { out.set(a, off); off += a.length; }
    }
    return out;
  }

  // 构建 multipart/form-data 请求体字节（供 SproxySig body 预哈希与发送）。
  // fields 为普通字段 {name: value}；fileField 为文件字段
  // {name, filename, contentType, bytes}。返回 {body: Uint8Array, contentType}。
  // 语义与 Go 端 multipart 一致（boundary 随机）。
  function buildMultipart(fields, fileField) {
    const boundary = '----WebKitFormBoundary' + crypto.getRandomValues(new Uint32Array(1))[0].toString(36);
    const parts = [];
    for (const key of Object.keys(fields || {})) {
      parts.push(TE.encode('--' + boundary + '\r\nContent-Disposition: form-data; name="' + key + '"\r\n\r\n' + fields[key] + '\r\n'));
    }
    if (fileField) {
      parts.push(TE.encode('--' + boundary + '\r\nContent-Disposition: form-data; name="' + fileField.name + '"; filename="' + String(fileField.filename).replace(/"/g, '') + '"\r\nContent-Type: ' + fileField.contentType + '\r\n\r\n'));
      parts.push(fileField.bytes);
      parts.push(TE.encode('\r\n'));
    }
    parts.push(TE.encode('--' + boundary + '--\r\n'));
    let total = 0;
    for (const p of parts) total += p.byteLength;
    const body = new Uint8Array(total);
    let off = 0;
    for (const p of parts) { body.set(p, off); off += p.byteLength; }
    return { body: body, contentType: 'multipart/form-data; boundary=' + boundary };
  }

  return {
    decodeJSON,
    encodeJSON,
    concatBytes,
    buildMultipart,
  };
});
