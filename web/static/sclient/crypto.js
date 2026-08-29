/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self, crypto */
/*
 * crypto.js —— sclient 前端库的 WebCrypto 基础工具。
 *
 * 纯 WebCrypto（crypto.subtle），不引入任何 npm 依赖，浏览器中暴露全局
 * sclientCrypto；Node 中通过 module.exports 导出，供单测使用。
 *
 * 注意：本文件只提供字节级/基础加解密能力，隧道帧组装（4B 长度 + nonce|ct
 * 的 application/x-tunnel-frame）在任务 4 的 transport.js 中实现，不在 crypto.js
 * 做帧。
 *
 * 与 Go 端对齐的已知向量（任务 2 注入值）：
 *   - HKDF：salt="sproxy-tunnel-key-v1"，info=mesh，派生 32B 隧道密钥
 *   - SproxySig canonical："sproxy-sig/v1\n" + ak + \n + ts + \n + exp + \n +
 *     nonce + \n + method + \n + path + \n + query + \n + body_sha256
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.sclientCrypto = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  const te = new TextEncoder();

  // hex 字符串 → Uint8Array。非法字符按 NaN→0 处理（调用方保证合法输入）。
  function hexToBytes(hex) {
    if (typeof hex !== 'string' || (hex.length % 2) !== 0) return new Uint8Array(0);
    const bytes = new Uint8Array(hex.length / 2);
    for (let i = 0; i < hex.length; i += 2) {
      bytes[i / 2] = parseInt(hex.substring(i, i + 2), 16);
    }
    return bytes;
  }

  // Uint8Array → hex 小写字符串。
  function bytesToHex(bytes) {
    const arr = new Uint8Array(bytes);
    let out = '';
    for (let i = 0; i < arr.length; i++) {
      out += arr[i].toString(16).padStart(2, '0');
    }
    return out;
  }

  // 归一化输入：string（UTF-8 编码）或 Uint8Array（原样使用）。
  function asBytes(input) {
    if (typeof input === 'string') return te.encode(input);
    if (input instanceof Uint8Array) return input;
    if (input && input.buffer && input.byteLength !== undefined) return new Uint8Array(input.buffer, input.byteOffset, input.byteLength);
    throw new TypeError('expected string or Uint8Array');
  }

  // SHA-256，返回 hex。输入 string（按 UTF-8 编码）或 Uint8Array。
  async function sha256Hex(input) {
    const digest = await crypto.subtle.digest('SHA-256', asBytes(input));
    return bytesToHex(new Uint8Array(digest));
  }

  // HMAC-SHA256：secret 为 hex 字符串，data 为 string 或 Uint8Array，返回 hex。
  // 注意：secret 字符串直接按 UTF-8 参与签名（Web UI app.js hmacSHA256Hex 的既有约定，
  // 与 WebCrypto importKey 的 string 入参一致；Go sproxysig.Sign 用 []byte(sk) 同一字节），
  // 不要 hex-decode。
  async function hmacSHA256Hex(secret, data) {
    const key = await crypto.subtle.importKey('raw', te.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
    const sig = await crypto.subtle.sign('HMAC', key, asBytes(data));
    return bytesToHex(new Uint8Array(sig));
  }

  // HKDF 派生隧道密钥（与 Go tunnel.DeriveTunnelKey 对齐）：
  //   salt = utf8("sproxy-tunnel-key-v1")，info = utf8(mesh)，输出 32B 密钥。
  // 返回 Uint8Array（32 字节）。
  async function deriveTunnelKey(secretHex, mesh) {
    const secret = hexToBytes(secretHex);
    if (secret.length !== 32) throw new Error('Tunnel 父密钥必须为 32 字节（64 hex）');
    const ikm = await crypto.subtle.importKey('raw', secret, 'HKDF', false, ['deriveBits']);
    const bits = await crypto.subtle.deriveBits(
      {
        name: 'HKDF',
        hash: 'SHA-256',
        salt: te.encode('sproxy-tunnel-key-v1'),
        info: te.encode(mesh || ''),
      },
      ikm,
      256
    );
    return new Uint8Array(bits);
  }

  // hex 密钥 → AES-GCM CryptoKey（32B，AES-256）。
  async function importAesGcmKey(secretHex) {
    const raw = hexToBytes(secretHex);
    if (raw.length === 0) throw new Error('AES-GCM 密钥不能为空');
    return crypto.subtle.importKey('raw', raw, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  }

  // AES-GCM 加密单块，返回 {iv, ciphertext}。iv 为 12B 随机。
  async function aesGcmEncrypt(key, plainBytes) {
    const plain = asBytes(plainBytes);
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, plain);
    return { iv, ciphertext: new Uint8Array(ct) };
  }

  // AES-GCM 解密单块，返回 Uint8Array。认证失败抛错（不吞异常）。
  async function aesGcmDecrypt(key, iv, ctBytes) {
    const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv }, key, asBytes(ctBytes));
    return new Uint8Array(plain);
  }

  // Node 下探活 WebCrypto（v24 已全局可用；显式检查避免环境差异）。
  // 浏览器（Chromium/Firefox/Safari 现代版本）与 Node 18+ 均内置 WebCrypto，
  // 此分支基本是不可达的防御性检查：只有老到不支持 crypto.subtle 的
  //（预 2017）浏览器或极老 Node 才会触发。加载期抛错是刻意为之——该库
  // 理论上无法在无 WebCrypto 环境下工作，尽早显式失败可避免后续每个调用
  // 都抛晦涩的 OperationError，这是无害的（不会加载失败导致页面白屏以外
  // 的副作用；页面由 index.html 静态加载各脚本，脚本加载失败浏览器会直接
  // 跳过并继续执行后续资源）。
  const cryptoObj = globalThis.crypto;
  if (!cryptoObj || !cryptoObj.subtle) {
    throw new Error('WebCrypto (crypto.subtle) 不可用——sclient 前端库要求现代浏览器或 Node 18+');
  }

  return {
    hexToBytes,
    bytesToHex,
    sha256Hex,
    hmacSHA256Hex,
    deriveTunnelKey,
    importAesGcmKey,
    aesGcmEncrypt,
    aesGcmDecrypt,
  };
});
