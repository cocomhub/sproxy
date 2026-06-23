// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 隧道加解密工具。依赖 sha256.js（先加载）。

var tunnelHexKey = localStorage.getItem('sproxy_tunnel_key') || '';
var _tunnelCryptoKey = null;

async function getTunnelCryptoKey() {
  if (!tunnelHexKey) return null;
  if (_tunnelCryptoKey) return _tunnelCryptoKey;
  var raw = hexToBytes(tunnelHexKey);
  if (raw.length !== 32) {
    showToast('Tunnel Key 必须为 64 位 hex', 'error');
    return null;
  }
  _tunnelCryptoKey = await crypto.subtle.importKey('raw', raw, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  return _tunnelCryptoKey;
}

function hexToBytes(hex) {
  var bytes = new Uint8Array(hex.length / 2);
  for (var i = 0; i < hex.length; i += 2) {
    bytes[i / 2] = Number.parseInt(hex.substring(i, i + 2), 16);
  }
  return bytes;
}

function bytesToHex(bytes) {
  return Array.from(new Uint8Array(bytes)).map(function(b) { return b.toString(16).padStart(2, '0'); }).join('');
}

// 构造并发送隧道请求，返回完整的解密后响应。
async function tunnelRequest(method, urlPath, headersObj, bodyBytes) {
  var key = await getTunnelCryptoKey();
  if (!key) throw new Error('未配置有效的 Tunnel Key');

  var meta = { method: method, url: urlPath, headers: {} };
  if (headersObj) {
    for (var entry of Object.entries(headersObj)) {
      var k = entry[0], v = entry[1];
      if (k !== 'Authorization') meta.headers[k] = v;
    }
  }

  // 加密 metadata
  var metaJSON = new TextEncoder().encode(JSON.stringify(meta));
  var metaIV = crypto.getRandomValues(new Uint8Array(12));
  var metaCT = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: metaIV }, key, metaJSON);
  var metaFrame = new Uint8Array(4 + metaIV.length + metaCT.byteLength);
  var dv = new DataView(metaFrame.buffer);
  dv.setUint32(0, metaIV.length + metaCT.byteLength, false);
  metaFrame.set(metaIV, 4);
  metaFrame.set(new Uint8Array(metaCT), 4 + metaIV.length);

  // 加密 body
  var bodyFrame = new Uint8Array(0);
  if (bodyBytes && bodyBytes.byteLength > 0) {
    var bodyIV = crypto.getRandomValues(new Uint8Array(12));
    var bodyCT = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: bodyIV }, key, bodyBytes);
    bodyFrame = new Uint8Array(4 + bodyIV.length + bodyCT.byteLength);
    var bdv = new DataView(bodyFrame.buffer);
    bdv.setUint32(0, bodyIV.length + bodyCT.byteLength, false);
    bodyFrame.set(bodyIV, 4);
    bodyFrame.set(new Uint8Array(bodyCT), 4 + bodyIV.length);
  }

  // 合并并发送
  var fullBody = new Uint8Array(metaFrame.length + bodyFrame.length);
  fullBody.set(metaFrame, 0);
  fullBody.set(bodyFrame, metaFrame.length);

  var resp = await fetch(BASE + '/tunnel', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-tunnel-frame' },
    body: fullBody
  });
  if (!resp.ok) {
    throw new Error('隧道请求失败: HTTP ' + resp.status);
  }

  // 解密响应
  var respArrayBuf = await resp.arrayBuffer();
  var respBytes = new Uint8Array(respArrayBuf);

  if (respBytes.length < 4) throw new Error('响应数据不足');
  var respMetaLen = new DataView(respBytes.buffer, respBytes.byteOffset).getUint32(0, false);
  var respMetaEnc = respBytes.slice(4, 4 + respMetaLen);

  var respMetaPlain = await decryptAESGCM(key, respMetaEnc.slice(0, 12), respMetaEnc.slice(12));
  var respMeta = JSON.parse(new TextDecoder().decode(respMetaPlain));

  // 解密 body 帧
  var bodyStart = 4 + respMetaLen;
  var respBody = await decryptBodyFrames(key, respBytes, bodyStart);

  return {
    status: respMeta.status,
    headers: respMeta.headers || {},
    body: respBody
  };
}

// 辅助：解密 AES-GCM 单帧
async function decryptAESGCM(key, iv, data) {
  return new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: iv }, key, data));
}

// 辅助：从响应字节中解密所有 body 帧
async function decryptBodyFrames(key, respBytes, startOffset) {
  var offset = startOffset;
  var chunks = [];
  while (offset + 4 <= respBytes.length) {
    var bLen = new DataView(respBytes.buffer, respBytes.byteOffset + offset).getUint32(0, false);
    if (bLen === 0 || offset + 4 + bLen > respBytes.length) break;
    var bEnc = respBytes.slice(offset + 4, offset + 4 + bLen);
    if (bEnc.length >= 12) {
      var chunk = await decryptAESGCM(key, bEnc.slice(0, 12), bEnc.slice(12));
      chunks.push(chunk);
    }
    offset += 4 + bLen;
  }
  var totalLen = chunks.reduce(function(s, c) { return s + c.length; }, 0);
  var allData = new Uint8Array(totalLen);
  var off = 0;
  for (var chunk of chunks) {
    allData.set(chunk, off);
    off += chunk.length;
  }
  return allData;
}

// --- 流式隧道下载 ---
async function tunnelDownloadStream(name) {
  var key = await getTunnelCryptoKey();
  if (!key) throw new Error('未配置有效的 Tunnel Key');

  var meta = { method: 'GET', url: '/download?filename=' + encodeURIComponent(name), headers: {} };
  var metaJSON = new TextEncoder().encode(JSON.stringify(meta));
  var metaIV = crypto.getRandomValues(new Uint8Array(12));
  var metaCT = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: metaIV }, key, metaJSON);
  var metaFrame = new Uint8Array(4 + metaIV.length + metaCT.byteLength);
  var dv = new DataView(metaFrame.buffer);
  dv.setUint32(0, metaIV.length + metaCT.byteLength, false);
  metaFrame.set(metaIV, 4);
  metaFrame.set(new Uint8Array(metaCT), 4 + metaIV.length);

  var resp = await fetch(BASE + '/tunnel', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-tunnel-frame' },
    body: metaFrame
  });
  if (!resp.ok) throw new Error('隧道请求失败: HTTP ' + resp.status);
  if (!resp.body) return null;

  var reader = resp.body.getReader();
  var metaLenBytes = await readNBytes(reader, 4);
  var metaLen = new DataView(metaLenBytes.buffer).getUint32(0, false);
  var metaEnc = await readNBytes(reader, metaLen);
  var metaPlain = await decryptAESGCM(key, metaEnc.slice(0, 12), metaEnc.slice(12));
  var respMeta = JSON.parse(new TextDecoder().decode(metaPlain));

  var chunks = [];
  var remainder = new Uint8Array(0);

  while (true) {
    remainder = await fillBuffer(reader, remainder, 4);
    if (remainder.length < 4) break;

    var chunkLen = new DataView(remainder.buffer, remainder.byteOffset).getUint32(0, false);
    if (chunkLen === 0) break;

    remainder = await fillBuffer(reader, remainder, 4 + chunkLen);
    if (remainder.length < 4 + chunkLen) break;

    var frameData = remainder.slice(4, 4 + chunkLen);
    remainder = remainder.slice(4 + chunkLen);

    if (frameData.length >= 12) {
      var plain = await decryptAESGCM(key, frameData.slice(0, 12), frameData.slice(12));
      chunks.push(plain);
    }
  }

  var totalLen = chunks.reduce(function(s, c) { return s + c.length; }, 0);
  var allData = new Uint8Array(totalLen);
  var off = 0;
  for (var chunk of chunks) {
    allData.set(chunk, off);
    off += chunk.length;
  }

  return {
    status: respMeta.status,
    headers: respMeta.headers || {},
    body: allData
  };
}

// 从 ReadableStream 中读取指定字节数
async function readNBytes(reader, n) {
  var buf = new Uint8Array(0);
  while (buf.length < n) {
    var result = await reader.read();
    if (result.done) throw new Error('响应数据不足');
    var tmp = new Uint8Array(buf.length + result.value.length);
    tmp.set(buf, 0);
    tmp.set(result.value, buf.length);
    buf = tmp;
  }
  return buf;
}

// 确保 remainder 中至少有 n 字节
async function fillBuffer(reader, remainder, n) {
  while (remainder.length < n) {
    var result = await reader.read();
    if (result.done) break;
    var tmp = new Uint8Array(remainder.length + result.value.length);
    tmp.set(remainder, 0);
    tmp.set(result.value, remainder.length);
    remainder = tmp;
  }
  return remainder;
}
