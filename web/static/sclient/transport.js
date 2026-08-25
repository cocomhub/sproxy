/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self, crypto */
/*
 * transport.js —— sclient 前端库的传输核心：有效模式解析 + 隧道/直连统一入口。
 *
 * 归一接口：coreRequest(method, pathWithQuery, {headers, bodyBytes, download}) → Promise<{status, headers, body(Uint8Array)}>。
 * 有效模式 effectiveMode()：'tunnel' | 'direct'：
 *   - 本地 override（localStorage overrideKey，transport 显式值 'direct'/'tunnel'）直接覆盖；
 *   - 否则跟随服务端 web.tunnel 开关（config.tunnelDefault，由 /api/config 下发后经
 *     transport.configure({tunnelDefault}) 写入——本模块只消费读取、不下发/更新）。
 *
 * 隧道模式（对齐 Go pkg/tunnel）：
 *   - 密钥 = deriveTunnelKey(secret, accessKeyMesh(ak))（HKDF，salt 固定 / info=mesh）；
 *   - 帧协议 application/x-tunnel-frame：[4B BE metaLen + AES-GCM(metaJSON)] + stream chunks
 *     （[4B BE chunkLen + nonce(12) + ct + tag]）；AES 上下文标签（AAD）与 Go 一致——
 *     metadata 帧 AADMeta "tunnel:meta:v1"，body 帧 AADStream "tunnel:stream:v1"，
 *     crypto.js 的 aesGcm* 不传 AAD；本模块用 WebCrypto 显式传 additionalData，
 *     与服务端 gcm.Seal(nonce, plaintext, aad) 同一字节语义。
 *   - 外层 /tunnel 签名用 sig.signHeader("POST", "/tunnel", 帧bytes, {unsigned:true})
 *     ——body_sha256=UNSIGNED（帧密文长度未知无法整体预哈希，与 Go sigRoundTripper 一致）。
 *
 * 依赖（浏览器按序挂全局 / Node require）：crypto.js→sclientCrypto、sig.js→sclientSig、
 * config.js→sclientConfig、log.js→sclientLog。
 * 另导出 SclientError（设计稿 §6 错误码）与 accessKeyMesh / configure / overrideKey，
 * 供领域方法与单测使用。旧 tunnel.js 由后续任务移除。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('./crypto.js'), require('./sig.js'), require('./config.js'), require('./log.js'));
  } else {
    root.sclientTransport = factory(root.sclientCrypto, root.sclientSig, root.sclientConfig, root.sclientLog);
  }
})(typeof self !== 'undefined' ? self : this, function (cryptoLib, sigLib, configLib, logLib) {
  'use strict';

  // ---- 常量（对齐 Go tunnel const）----
  const frameContentType = 'application/x-tunnel-frame';
  const AAD_META = 'tunnel:meta:v1';
  const AAD_STREAM = 'tunnel:stream:v1';
  const MAX_META = 1 << 20; // Go MaxMetadataBytes：1 MiB
  const TUNNEL_CHUNK_SIZE = 64 * 1024; // Go DefaultChunkSize：单 body 帧明文块 64 KB
  const TE = new TextEncoder();
  const TD = new TextDecoder();
  // AAD 上下文字节（预编码一次，减少重复 TextEncoder 调用）。
  const AAD_META_BYTES = TE.encode(AAD_META);
  const AAD_STREAM_BYTES = TE.encode(AAD_STREAM);

  // ---- 可调状态（configure 注入，默认取 config.defaultConfig）----
  let cfg = configLib.defaultConfig();

  // 用 patch 覆盖本模块状态；支持领域方法把 /api/config 下发的 web_tunnel 与
  // localStorage override 落地到这里：
  //   - tunnelDefault（服务端 web.tunnel；忽略 undefined/null）
  //   - accessKey / accessKeySecret（签名/派生凭证）
  //   - mode（'tunnel'|'direct'|'auto'——测试注入/页面强制；非显式给值时忽略）
  // 其余字段保留默认。返回合并后的配置副本。
  function configure(patch) {
    if (!patch || typeof patch !== 'object') return Object.assign({}, cfg);
    const allowed = ['baseUrl', 'accessKey', 'accessKeySecret', 'transport', 'tunnelDefault', 'overrideKey'];
    for (const key of allowed) {
      const val = patch[key];
      if (val === undefined || val === null) continue;
      if (key === 'overrideKey' && (typeof val !== 'string' || val === '')) continue;
      if (key === 'accessKeySecret' && (typeof val !== 'string' || val === '')) continue;
      cfg[key] = val;
    }
    // mode 是 configure 特有的运行时强制项，不与 config.defaultConfig 的 transport 混淆。
    // 注入 'auto'（或省略）时回落 cfg.transport（由 config default 或 override 决定）。
    const modePatch = patch.mode;
    if (modePatch === 'tunnel' || modePatch === 'direct') {
      cfg.mode = modePatch;
    } else if (modePatch === 'auto' || modePatch === undefined || modePatch === null) {
      cfg.mode = cfg.transport;
    }
    return Object.assign({}, cfg);
  }

  // ---- SclientError：库内统一错误（设计稿 §6）。status 可缺省。 ----
  function SclientError(code, message, status) {
    const err = new Error(message);
    err.name = 'SclientError';
    err.code = code;
    if (status !== undefined) err.status = status;
    return err;
  }

  // ---- AccessKeyMesh：Go tunnel.AccessKeyMesh 的 JS 移植（I-1 唯一实现） ----
  // sk-<mesh>-<16hex>（mesh 可含连字符，取最后一个 '-'）→ mesh；
  // sk-<16hex>（无 mesh 段）→ ''；格式不合法 → ''。
  function accessKeyMesh(ak) {
    if (typeof ak !== 'string' || ak.indexOf('sk-') !== 0) return '';
    const rest = ak.slice(3);
    const idx = rest.lastIndexOf('-');
    if (idx <= 0 || idx + 17 !== rest.length) return '';
    const hexPart = rest.slice(idx + 1);
    if (!/^[0-9a-fA-F]{16}$/.test(hexPart)) return '';
    return rest.slice(0, idx);
  }

  // overrideKey 回调（测试/诊断用）——localStorage 键由 config 提供，
  // 领域层可据此读写覆盖；本模块 effectiveMode 实时调用读它。
  function overrideKey() {
    return cfg.overrideKey;
  }

  // rootURL + 相对 pathWithQuery → 完整 URL（对齐 Go doRequest 的 serverURL+urlPath 拼接；
  // baseUrl 为空时直接用相对路径给 fetch——同源部署语义）。
  function fullURL(pathWithQuery) {
    if (!cfg.baseUrl) return pathWithQuery;
    return cfg.baseUrl.replace(/\/+$/, '') + pathWithQuery;
  }

  // ---- 有效模式 ----
  // 优先级：localStorage override（transport 显式值）> 服务端 web.tunnel。
  // mode 字段（configure 注入）只在非空值（tunnel/direct）时参与——供测试/页面强制。
  function effectiveMode() {
    const o = configLib.readLocalOverride();
    const overrideTransport = o.transport;
    if (overrideTransport === 'tunnel' || overrideTransport === 'direct') return overrideTransport;
    // 无 override：跟随服务端开关
    if (cfg.mode === 'tunnel' || cfg.mode === 'direct') return cfg.mode;
    return cfg.tunnelDefault ? 'tunnel' : 'direct';
  }

  // ================= 隧道模式 =================

  // deriveTunnelKeyHex：SK(64 hex) + mesh → 隧道密钥 hex（缓存）。mesh 由
  // accessKeyMesh(ak) 统一解析；ak/sk 变更时 key 变化，缓存以字符串为键不泄漏。
  const keyCache = new Map();
  async function getTunnelKeyHex(ak, sk) {
    const mesh = accessKeyMesh(ak);
    const cacheKey = ak + '\0' + sk + '\0' + mesh;
    let v = keyCache.get(cacheKey);
    if (v === undefined) {
      const key = await cryptoLib.deriveTunnelKey(sk, mesh);
      v = cryptoLib.bytesToHex(key);
      keyCache.set(cacheKey, v);
    }
    return v;
  }

  // 加密 single 块：返回 [iv(12) | ct+tag]（密文不含长度前缀；长度由调用方拼）。
  async function encryptBlockAAD(keyHex, plainbytes, aad) {
    const keyObj = await cryptoLib.importAesGcmKey(keyHex);
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: aad }, keyObj, plainbytes);
    return concatU8(iv, new Uint8Array(ct));
  }

  // 解密 single 块：enc = [iv(12) | ct+tag]（不含长度前缀）。失败抛错给上层归一 E_DECRYPT。
  async function decryptBlockAAD(keyHex, enc, aad) {
    const keyObj = await cryptoLib.importAesGcmKey(keyHex);
    if (enc.length < 12) throw SclientError('E_DECRYPT', '响应加密块过短');
    const iv = enc.subarray(0, 12);
    const ct = enc.subarray(12);
    const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv, additionalData: aad }, keyObj, ct);
    return new Uint8Array(plain);
  }

  // 构造 metadata 帧 [4B len + iv(12) + ct+tag]。metaJSON 用 TextEncoder 编码为 bytes。
  async function encodeMetadataFrame(keyHex, metaObject) {
    const jsonBytes = TE.encode(JSON.stringify(metaObject));
    const enc = await encryptBlockAAD(keyHex, jsonBytes, AAD_META_BYTES);
    return frameFromEnc(enc);
  }

  // 构造 body 帧 array：[frame, frame, ...]（每帧各含 4B 前缀）；返回帧字节序列。
  async function encodeBodyFrames(keyHex, bodyBytes) {
    const b = bodyBytes || new Uint8Array(0);
    const frames = [];
    for (let off = 0; off < b.length; off += TUNNEL_CHUNK_SIZE) {
      const chunk = b.subarray(off, off + TUNNEL_CHUNK_SIZE);
      const enc = await encryptBlockAAD(keyHex, chunk, AAD_STREAM_BYTES);
      frames.push(concatU8(u32be(enc.length), enc));
    }
    return frames;
  }

  // [4B BE len + enc] 组帧。
  function frameFromEnc(enc) {
    return concatU8(u32be(enc.length), enc);
  }

  function u32be(n) {
    const b = new Uint8Array(4);
    new DataView(b.buffer).setUint32(0, n, false);
    return b;
  }

  function concatU8() {
    let total = 0;
    for (let i = 0; i < arguments.length; i++) total += arguments[i] ? arguments[i].length : 0;
    const out = new Uint8Array(total);
    let off = 0;
    for (let i = 0; i < arguments.length; i++) {
      const a = arguments[i];
      if (a) { out.set(a, off); off += a.length; }
    }
    return out;
  }

  // 从字节序列解析响应：读 [4B metaLen + meta密文]，再循环读 body 帧。
  // 返回 {status, headers, body(Uint8Array)}。任一步失败 → E_DECRYPT。
  async function decodeResponseFrames(keyHex, bytes) {
    let view = null;
    if (bytes.length < 4) throw SclientError('E_DECRYPT', '响应数据不足（缺 4B meta 长度）');
    view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const metaLen = view.getUint32(0, false);
    if (metaLen > MAX_META) throw SclientError('E_DECRYPT', 'metadata 帧过长');
    if (4 + metaLen > bytes.length) throw SclientError('E_DECRYPT', '响应 metadata 数据不足');
    const metaEnc = bytes.subarray(4, 4 + metaLen);
    let metaPlain;
    try {
      metaPlain = await decryptBlockAAD(keyHex, metaEnc, AAD_META_BYTES);
    } catch (e) {
      if (e && e.code === 'E_DECRYPT') throw e;
      throw SclientError('E_DECRYPT', '响应 metadata 解密失败');
    }
    let respMeta;
    try {
      respMeta = JSON.parse(TD.decode(metaPlain));
    } catch (e) {
      throw SclientError('E_DECRYPT', '响应 metadata JSON 解析失败');
    }

    // body 帧循环（与 Go DecryptStream 语义一致：读 4B 长度、跳过空帧、解密）。
    const chunks = [];
    let offset = 4 + metaLen;
    while (offset + 4 <= bytes.length) {
      const bLen = view.getUint32(offset, false);
      if (bLen === 0) { offset += 4; continue; }
      if (bLen > 1 << 20) throw SclientError('E_DECRYPT', 'stream chunk 过长');
      if (offset + 4 + bLen > bytes.length) throw SclientError('E_DECRYPT', 'stream chunk 数据不足');
      const bEnc = bytes.subarray(offset + 4, offset + 4 + bLen);
      try {
        chunks.push(await decryptBlockAAD(keyHex, bEnc, AAD_STREAM_BYTES));
      } catch (e) {
        if (e && e.code === 'E_DECRYPT') throw e;
        throw SclientError('E_DECRYPT', '响应 body 帧解密失败');
      }
      offset += 4 + bLen;
    }

    const body = concatU8.apply(null, chunks);
    return {
      status: (respMeta && typeof respMeta.status === 'number') ? respMeta.status : 200,
      headers: (respMeta && respMeta.headers) || {},
      body: body,
    };
  }

  // ---- 隧道请求（buffered）----
  async function tunnelRun(method, pathWithQuery, opts) {
    const { headers, bodyBytes } = opts || {};
    const ak = cfg.accessKey;
    const sk = cfg.accessKeySecret;

    // 派生隧道密钥（mesh 从 AK 提取；与 Go authMiddleware AK→HKDF 派生一致）。
    let keyHex;
    try {
      if (!sk) throw SclientError('E_AUTH', '隧道模式需要 accessKeySecret 派生密钥');
      keyHex = await getTunnelKeyHex(ak, sk);
    } catch (e) {
      if (e && e.code) throw e;
      throw SclientError('E_AUTH', '隧道密钥派生失败');
    }

    // metadata：method/url 相对路径/headers（去掉 Authorization——外层有独立签名）
    const meta = { method: method, url: pathWithQuery, headers: {} };
    if (headers) {
      for (const [k, v] of Object.entries(headers)) {
        if (k.toLowerCase() !== 'authorization' && typeof v === 'string') meta.headers[k] = v;
      }
    }
    const metaFrame = await encodeMetadataFrame(keyHex, meta);
    const bodyFrames = await encodeBodyFrames(keyHex, bodyBytes);
    const fullBody = concatU8(metaFrame, bodyFrames.length ? concatU8.apply(null, bodyFrames) : new Uint8Array(0));

    // 外层签名：body_sha256=UNSIGNED（帧密文无法整体预哈希），与 Go sigRoundTripper 一致。
    const auth = await sigLib.signHeader('POST', '/tunnel', fullBody, {
      ak: cfg.accessKey,
      secret: cfg.accessKeySecret,
      unsigned: true,
    });

    let resp;
    try {
      resp = await globalThis.fetch(fullURL('/tunnel'), {
        method: 'POST',
        headers: { 'Content-Type': frameContentType, 'Authorization': auth },
        body: fullBody,
      });
    } catch (e) {
      const err = (e && e.code) ? e : SclientError('E_NETWORK', '网络错误：' + (e && e.message ? e.message : e), undefined);
      if (err.code) { err.cause = e; }
      throw err;
    }
    if (resp.status === 401 || resp.status === 403) {
      throw SclientError('E_AUTH', '认证失败（HTTP ' + resp.status + '）', resp.status);
    }
    if (!resp.ok) throw SclientError('E_SERVER', '隧道请求失败（HTTP ' + resp.status + '）', resp.status);

    // 下载特例：外层流式读取 + 逐帧解密（不整体缓冲）。否则 arrayBuffer 后统一解密。
    if (opts && opts.download) {
      if (!resp.body) throw SclientError('E_DECRYPT', '响应无流');
      return await streamDecode(keyHex, resp);
    }

    const buf = new Uint8Array(await resp.arrayBuffer());
    return await decodeResponseFrames(keyHex, buf);
  }

  // ---- 流式响应解码（ReadableStream.getReader 逐帧） ----
  // 下载分支（opts.download）直接消费 resp.body.getReader()（见 readNBytes/fillBuffer
  // 与下方 streamDecode），不在此包装 makeByteSource。

  // 从 ReadableStream 读满 n 字节；不足/提前 EOF 抛 E_DECRYPT。
  async function readNBytes(reader, n) {
    let buf = new Uint8Array(0);
    while (buf.length < n) {
      const { done, value } = await reader.read();
      if (done) throw SclientError('E_DECRYPT', '响应流提前结束');
      buf = concatU8(buf, value instanceof Uint8Array ? value : new Uint8Array(await value.arrayBuffer()));
    }
    return buf;
  }

  // 逐帧解码（与 tunnel.js tunnelDownloadStream 相同策略，密钥来自派生）。
  async function streamDecode(keyHex, resp) {
    const reader = resp.body.getReader();
    let metaLenBytes;
    try {
      metaLenBytes = await readNBytes(reader, 4);
    } catch (e) { throw e; } // E_DECRYPT 透传
    const metaLen = new DataView(metaLenBytes.buffer).getUint32(0, false);
    if (metaLen > MAX_META) throw SclientError('E_DECRYPT', 'metadata 帧过长');
    const metaEnc = await readNBytes(reader, metaLen);
    let metaPlain;
    try {
      metaPlain = await decryptBlockAAD(keyHex, metaEnc, AAD_META_BYTES);
    } catch (e) {
      if (e && e.code === 'E_DECRYPT') throw e;
      throw SclientError('E_DECRYPT', '响应 metadata 解密失败');
    }
    let respMeta;
    try {
      respMeta = JSON.parse(TD.decode(metaPlain));
    } catch (e) {
      throw SclientError('E_DECRYPT', '响应 metadata JSON 解析失败');
    }

    const chunks = [];
    let remainder = new Uint8Array(0);
    for (;;) {
      remainder = await fillBuffer(reader, remainder, 4);
      if (remainder.length < 4) break;
      const chunkLen = new DataView(remainder.buffer, remainder.byteOffset).getUint32(0, false);
      if (chunkLen === 0) { remainder = remainder.subarray(4); continue; }
      if (chunkLen > 1 << 20) throw SclientError('E_DECRYPT', 'stream chunk 过长');
      remainder = await fillBuffer(reader, remainder, 4 + chunkLen);
      if (remainder.length < 4 + chunkLen) break;
      const frameData = remainder.subarray(4, 4 + chunkLen);
      remainder = remainder.subarray(4 + chunkLen);
      try {
        chunks.push(await decryptBlockAAD(keyHex, frameData, AAD_STREAM_BYTES));
      } catch (e) {
        if (e && e.code === 'E_DECRYPT') throw e;
        throw SclientError('E_DECRYPT', '响应 body 帧解密失败');
      }
    }

    return {
      status: (respMeta && typeof respMeta.status === 'number') ? respMeta.status : 200,
      headers: (respMeta && respMeta.headers) || {},
      body: concatU8.apply(null, chunks),
    };
  }

  async function fillBuffer(reader, remainder, n) {
    while (remainder.length < n) {
      const result = await reader.read();
      if (result.done) break;
      remainder = concatU8(remainder, result.value instanceof Uint8Array ? result.value : new Uint8Array(await result.value.arrayBuffer()));
    }
    return remainder;
  }

  // ================= 直连模式 =================
  async function directRun(method, pathWithQuery, opts) {
    const { headers, bodyBytes } = opts || {};
    const ak = cfg.accessKey;
    const sk = cfg.accessKeySecret;
    // SproxySig（有 body 用其 SHA-256；无 body 签空串）——对齐 Go signRequest。
    let auth;
    try {
      auth = await sigLib.signHeader(method, pathWithQuery, bodyBytes || null, { ak, secret: sk });
    } catch (e) {
      throw SclientError('E_AUTH', '签名失败：' + (e && e.message), undefined);
    }
    const mergedHeaders = {};
    if (headers) {
      for (const [k, v] of Object.entries(headers)) {
        if (typeof v === 'string') mergedHeaders[k] = v;
      }
    }
    mergedHeaders.Authorization = auth;
    if (bodyBytes && bodyBytes.length > 0) mergedHeaders['Content-Type'] = mergedHeaders['Content-Type'] || 'application/octet-stream';

    let resp;
    try {
      resp = await globalThis.fetch(fullURL(pathWithQuery), {
        method: method,
        headers: mergedHeaders,
        body: bodyBytes && bodyBytes.length > 0 ? bodyBytes : undefined,
      });
    } catch (e) {
      const err = (e && e.code) ? e : SclientError('E_NETWORK', '网络错误：' + (e && e.message ? e.message : e), undefined);
      if (err.code) { err.cause = e; }
      throw err;
    }
    if (resp.status === 401 || resp.status === 403) {
      throw SclientError('E_AUTH', '认证失败（HTTP ' + resp.status + '）', resp.status);
    }
    if (!resp.ok) throw SclientError('E_SERVER', '请求失败（HTTP ' + resp.status + '）', resp.status);

    const bodyArr = new Uint8Array(await resp.arrayBuffer());
    const outHeaders = {};
    resp.headers && resp.headers.forEach ? resp.headers.forEach((v, k) => { outHeaders[k] = v; }) : (outHeaders['content-type'] = resp.headers.get('content-type'));
    return { status: resp.status, headers: outHeaders, body: bodyArr };
  }

  // 统一入口：解析有效模式 → 隧道/直连 → 归一 {status, headers, body(Uint8Array)}。
  // opts.download=true 时隧道分支用流式读取（direct 分支无差别 arrayBuffer）。
  async function coreRequest(method, pathWithQuery, opts) {
    const mode = effectiveMode();
    const actual = mode === 'tunnel' ? tunnelRun : directRun;
    return await actual(method, pathWithQuery, opts || {});
  }

  // 默认显式导出（浏览器经全局 sclientTransport 使用）。
  return {
    coreRequest,
    effectiveMode,
    accessKeyMesh,
    configure,
    overrideKey,
    SclientError,
    fullURL,
    decodeResponseFrames,
    // 内部原语（单测跨端闭环用；非公共 API，勿在领域层依赖）。
    _internals: {
      getTunnelKeyHex,
      encodeMetadataFrame,
      encodeBodyFrames,
      decryptBlockAAD,
      frameFromEnc,
      concatU8,
      u32be,
    },
  };
});
