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
    if (schemeEnd >= 0) {
      hostStart = schemeEnd + 3;
    } else if (rest.startsWith('//')) {
      hostStart = 2;
    } else {
      return null; // 无 host → Go url.Parse 的 Host 为空
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
  // 与 Go pkg/cloudfilename.Safe 一致：替换 \ / ? : < > | " * 与 NUL，
  // 去除首尾空白与点，抵御 Windows 保留设备名，并按字节截断。
  function filepathSafe(name) {
    name = name.replace(/\x00/g, '');
    name = name.replace(/[\\/:<>|"*?]/g, '_');
    name = name.replace(/^[ .]+|[ .]+$/g, '');
    if (!name) return 'download';
    const base = name.toUpperCase().split('.')[0];
    if (WIN_RESERVED[base]) name = '_' + name;
    name = truncateName(name, MAX_NAME_BYTES);
    if (!name) return 'download';
    return name;
  }

  // safeDefaultFromURL = genDefaultFilename + filepathSafe 一步完成
  function safeDefaultFromURL(rawUrl) {
    return filepathSafe(genDefaultFilename(rawUrl));
  }

  // validateEntry 校验 URL 格式（对齐 Go cloudfilename.ValidateEntry）
  function validateEntry(url) {
    if (!url) return 'URL is empty';
    const parsed = parseURL(url);
    if (!parsed) return 'unsupported URL scheme or missing host';
    if (!/^https?:\/\//i.test(url)) return 'unsupported URL scheme (only http/https)';
    return null; // 无错误
  }

  return { genDefaultFilename: genDefaultFilename, filepathSafe: filepathSafe, safeDefaultFromURL: safeDefaultFromURL, validateEntry: validateEntry };
});
