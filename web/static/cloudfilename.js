/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * cloudfilename.js —— 云端下载默认文件名生成与清理规则（浏览器端）。
 *
 * 与 Go 端 pkg/cloudfilename 保持同一套规则（wget 行为），通过共享语料
 * web/static/cloudfilename.test.js 保证双端一致：
 *   - 路径末尾为 / 时使用 "index.html"（如 /xx/?a=v → index.html?a=v）
 *   - 查询参数（? 后的 raw query）直接附加到文件名后（不做归一化/解码）
 *   - 路径最后一段做百分号解码（路径语义，不把 + 解码为空格）
 *
 * 浏览器中暴露全局 cloudfilename；Node 中通过 module.exports 导出，供单测使用。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.cloudfilename = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // parseURL 从原始字符串提取 scheme/host/rawPath/rawQuery/hash，对齐 Go url.Parse 语义：
  //   - host 必须非空（`://` 之后、`/` 或 `?` 或 `#` 或结尾之前）
  //   - rawPath 不归一化点段（/dir/.. 保留原样，WHATWG pathname 会折叠为 /）
  //   - rawQuery 不做任何解码/归一化（保留原始字节，含空格与中文）
  // 无 `://`（如 mailto:、data:、相对 URL、纯主机名）返回 null → 视为无 host。
  function parseURL(rawUrl) {
    let rest = rawUrl;
    const hashIdx = rest.indexOf('#');
    const hash = hashIdx >= 0 ? rest.slice(hashIdx) : '';
    if (hashIdx >= 0) rest = rest.slice(0, hashIdx);
    const qIdx = rest.indexOf('?');
    const rawQuery = qIdx >= 0 ? rest.slice(qIdx) : '';
    if (qIdx >= 0) rest = rest.slice(0, qIdx);
    // scheme 判定：`://`（http://host）或 `//`（protocol-relative，Go url.Parse 解析出
    // 无 scheme 但非空 Host）；其余（mailto:、data:、相对 URL、纯主机名）Host 均为空。
    let hostStart;
    const schemeEnd = rest.indexOf('://');
    let schemePrefix = ''; // scheme 前原文（用于检测前导空白，对齐 Go url.Parse 报错）
    if (schemeEnd >= 0) {
      hostStart = schemeEnd + 3;
      schemePrefix = rest.slice(0, schemeEnd);
    } else if (rest.startsWith('//')) {
      hostStart = 2;
    } else {
      return null; // 无 host → Go url.Parse 的 Host 为空
    }
    // 前导空白：Go url.Parse 会把整段前导空白解析进 Scheme（如 "  http"），
    // 随后因 first path segment contains colon 报错 → 默认返回 download；
    // JS 若忽略空白则会把空白留在 scheme prefix 之外正常解析出 'file'，两短分歧。
    // 这里显式对齐：scheme 前出现任何空白 → 视为无 host（download）。
    if (/^\s/.test(schemePrefix)) {
      return null;
    }
    const afterScheme = rest.slice(hostStart);
    const slashIdx = afterScheme.indexOf('/');
    let host, rawPath;
    if (slashIdx < 0) {
      host = afterScheme;
      rawPath = '';
    } else {
      host = afterScheme.slice(0, slashIdx);
      rawPath = afterScheme.slice(slashIdx);
    }
    if (!host) return null;
    // 剥离 userinfo（Go url.Parse 的 Host 不含 user:pass@ 前缀；userinfo 只影响解析不参与 Path）
    const atIdx = host.indexOf('@');
    let hostForValidation = host;
    if (atIdx >= 0) {
      hostForValidation = host.slice(atIdx + 1);
    }
    // host 中出现任何百分号 → Go url.Parse 报 invalid URL escape（host 不允许未解码
    // 转义），默认返回 download。注意 userinfo 部分（@ 前）允许百分号，不在此列。
    if (/%/.test(hostForValidation)) return null;
    // host 中 Go url.Parse 会报错的字符（实测）：空白/控制字符、|、反引号、非法端口。
    // 这些输入 Go 侧返回 download，JS 也必须拒绝，避免双端分歧。
    // 注意：用显式 ASCII 空白集合而非 \s——\s 会匹配 U+00A0/U+3000 等 Unicode 空白，
    // 而 Go url.Parse 接受这些字符作 host（实测 http://example.com /file → file）。
    // < > ' & ( ) $ ~ + _ ] ! * 等 Go 合法，不得拒绝；[ 单独报错但边界复杂，暂不处理。
    if (/[ \t\n\r\f\v|`]/.test(hostForValidation)) return null;
    // 非法端口（: 后非纯数字）→ Go url.Parse 报 invalid port。合法端口（如 :8080）保留。
    // 仅检查 [ 之后的冒号（IPv6 字面量 [::1] 内的冒号属于地址，不算端口）。
    const closeBracketIdx = hostForValidation.lastIndexOf(']');
    const colonIdx = hostForValidation.lastIndexOf(':');
    if (colonIdx > closeBracketIdx) {
      const port = hostForValidation.slice(colonIdx + 1);
      if (!/^\d*$/.test(port)) return null;
    }
    return { rawPath: rawPath, rawQuery: rawQuery, hash: hash };
  }

  // genDefaultFilename 从 URL 推断默认文件名，与 Go pkg/cloudfilename.DefaultFromURL 一致。
  // 返回文件名未经 filepathSafe 清理，调用方应自行调用 filepathSafe。
  function genDefaultFilename(rawUrl) {
    const parsed = parseURL(rawUrl);
    if (!parsed) return 'download';
    // 与 Go url.Parse 一致：非法百分号编码使解析失败 → "download"。
    // 注意：Go 只校验 path 与 fragment 的转义，query 原样保留；所以这里只检查
    // rawPath + hash，不检查 rawQuery（否则 ?x=100% 会误判为 download）。
    if (/%(?![0-9A-Fa-f]{2})/.test(parsed.rawPath + parsed.hash)) {
      return 'download';
    }
    // 与 Go url.Parse 一致：对原始路径做一次百分号解码（得到 decoded Path）
    let path;
    try {
      path = decodeURIComponent(parsed.rawPath);
    } catch (e) {
      return 'download';
    }
    // 去掉末尾的 /（只去掉一个，对应 strings.TrimSuffix(path, "/")）
    const trimmed = path.replace(/\/$/, '');
    if (!trimmed) {
      // 纯 / 路径
      let name = 'index.html';
      if (parsed.rawQuery) name += parsed.rawQuery;
      return name;
    }
    // 取最后一段
    let name = trimmed.substring(trimmed.lastIndexOf('/') + 1);
    // 二次百分号解码（对应 Go url.PathUnescape），失败则保持原样
    try {
      name = decodeURIComponent(name);
    } catch (e) { /* 保持原样 */ }
    // 原路径以 / 结尾（trimmed 比 path 短）→ index.html
    if (path.length > trimmed.length) {
      name = 'index.html';
    }
    // 查询参数附加在文件名后（raw query 原样保留）
    if (parsed.rawQuery) name += parsed.rawQuery;
    return name || 'download';
  }

  // ---- filepathSafe ----

  // MAX_NAME_BYTES 是文件名最大 UTF-8 字节数（与 Go maxNameBytes 一致）。
  const MAX_NAME_BYTES = 254;

  // WIN_RESERVED 是 Windows 保留设备名（基名匹配，大小写不敏感）。
  const WIN_RESERVED = {};
  ['CON', 'PRN', 'AUX', 'NUL',
    'COM1', 'COM2', 'COM3', 'COM4', 'COM5', 'COM6', 'COM7', 'COM8', 'COM9',
    'LPT1', 'LPT2', 'LPT3', 'LPT4', 'LPT5', 'LPT6', 'LPT7', 'LPT8', 'LPT9'
  ].forEach(function (n) { WIN_RESERVED[n] = true; });

  function utf8Bytes(str) {
    return new TextEncoder().encode(str).length;
  }

  // truncateBytes 按字节截断到 maxBytes，不劈开 UTF-8 字符（for...of 按 code point 迭代）。
  function truncateBytes(str, maxBytes) {
    if (utf8Bytes(str) <= maxBytes) return str;
    let out = '';
    let bytes = 0;
    for (const ch of str) {
      const len = utf8Bytes(ch);
      if (bytes + len > maxBytes) break;
      bytes += len;
      out += ch;
    }
    return out;
  }

  // truncateName 按字节截断，优先保留扩展名（与 Go truncateName 一致）。
  function truncateName(name, maxBytes) {
    if (utf8Bytes(name) <= maxBytes) return name;
    const dotIdx = name.lastIndexOf('.');
    if (dotIdx > 0) {
      const ext = name.slice(dotIdx);
      if (utf8Bytes(ext) < maxBytes) {
        const base = name.slice(0, dotIdx);
        if (base) return truncateBytes(base, maxBytes - utf8Bytes(ext)) + ext;
      }
    }
    return truncateBytes(name, maxBytes);
  }

  // filepathSafe 清理文件名中的路径分隔符，防止路径穿越。
  // 与 Go pkg/cloudfilename.Safe 一致：替换 \ / ? : < > | " * \t 与 NUL，
  // 去除首尾空白与点，抵御 Windows 保留设备名，并按字节截断。
  function filepathSafe(name) {
    name = name.replace(/\x00/g, '');
    name = name.replace(/[\\/:<>|"*?\t]/g, '_');
    name = name.replace(/^[ .]+|[ .]+$/g, '');
    if (!name) return 'download';
    const base = name.toUpperCase().split('.')[0];
    if (WIN_RESERVED[base]) name = '_' + name;
    name = truncateName(name, MAX_NAME_BYTES);
    if (!name) return 'download';
    return name;
  }

  // safeDefaultFromURL = genDefaultFilename + filepathSafe 一步完成
  // 注意：Go url.Parse 不做前导/尾随空白 trim —— 前导空白会让 host 解析失败
  //       （返回 download），而尾随空白会被当作路径的一部分，Safe 时会去除。
  //       因此这里不能全局 trim，避免破坏与 Go 的一致性；
  //       前导空白已在 parseURL 中按 Go 语义处理（→ download）。
  function safeDefaultFromURL(rawUrl) {
    return filepathSafe(genDefaultFilename(rawUrl));
  }

  // validateEntry 校验 URL 格式（对齐 Go cloudfilename.ValidateEntry）。
  // 返回结构化结果: { valid, code, message }。
  // 优先于 trim 前先判空（空串 → EMPTY_URL）；
  // 注意：Go url.Parse 对前导空白（host 解析失败）与尾随空白（并入路径）
  // 都各自有对应行为，这里不做 trim，保持与 Go 一致。
  function validateEntry(url) {
    if (!url) return { valid: false, code: 'EMPTY_URL', message: 'URL is empty' };
    const parsed = parseURL(url);
    if (!parsed) return { valid: false, code: 'BAD_SCHEME', message: 'unsupported URL scheme or missing host' };
    if (!/^https?:\/\//i.test(url)) return { valid: false, code: 'BAD_SCHEME', message: 'unsupported URL scheme (only http/https)' };
    return { valid: true, code: 'OK', message: '' };
  }

  // validateEntries 校验一组条目：全部通过 validateEntry，且同 URL 不允许出现
  // 不同 Filename（对齐 Go cloudfilename.ValidateEntries，ErrEntryDupURL）。
  // 返回首个错误结果: { valid, code, message }。
  function validateEntries(entries) {
    // 用 Map 而非普通对象做去重表：避免 URL 恰好是 constructor/__proto__ 等原型键时误判 DUP_URL。
    const urlFilenames = new Map();
    for (const e of entries) {
      const v = validateEntry(e.url);
      if (!v.valid) return v;
      if (urlFilenames.has(e.url) && urlFilenames.get(e.url) !== e.filename) {
        return { valid: false, code: 'DUP_URL', message: 'duplicate URL with different filename: ' + e.url };
      }
      urlFilenames.set(e.url, e.filename);
    }
    return { valid: true, code: 'OK', message: '' };
  }

  return { genDefaultFilename: genDefaultFilename, filepathSafe: filepathSafe, safeDefaultFromURL: safeDefaultFromURL, validateEntry: validateEntry, validateEntries: validateEntries };
});
