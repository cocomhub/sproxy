/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * cloudfilename.js —— 云端下载默认文件名生成与清理规则（浏览器端）。
 *
 * 与 Go 端 pkg/cloudfilename 保持同一套规则（wget 行为），通过共享语料
 * web/static/cloudfilename.test.js 保证双端一致：
 *   - 路径末尾为 / 时使用 "index.html"（如 /xx/?a=v → index.html?a=v）
 *   - 查询参数（? 后的 raw query）直接附加到文件名后
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

  // rawPathFromURL 从原始 URL 字符串提取"未归一化"的路径部分（对齐 Go url.Parse：
  // Go 不做 WHATWG 的点段折叠，/dir/.. 的 Path 仍是 /dir/..；而 new URL().pathname
  // 会把 /dir/.. 折叠为 /，导致 JS 取到 index.html 而 Go 取到 ".." 的分歧）。
  // 返回路径为原始百分号编码形式，调用方再按 Go 语义解码。
  function rawPathFromURL(rawUrl) {
    let rest = rawUrl;
    const hashIdx = rest.indexOf('#');
    if (hashIdx >= 0) rest = rest.slice(0, hashIdx);
    const qIdx = rest.indexOf('?');
    if (qIdx >= 0) rest = rest.slice(0, qIdx);
    const schemeEnd = rest.indexOf('://');
    if (schemeEnd < 0) return '';
    const afterScheme = rest.slice(schemeEnd + 3);
    const slashIdx = afterScheme.indexOf('/');
    if (slashIdx < 0) return '';
    return afterScheme.slice(slashIdx);
  }

  // genDefaultFilename 从 URL 推断默认文件名，与 Go pkg/cloudfilename.DefaultFromURL 一致。
  // 返回文件名未经 filepathSafe 清理，调用方应自行调用 filepathSafe。
  function genDefaultFilename(rawUrl) {
    let parsed;
    try {
      parsed = new URL(rawUrl);
    } catch (e) {
      return 'download';
    }
    // 与 Go url.Parse 一致：非法百分号编码使解析失败 → "download"。
    // 注意：Go 只校验 path 与 fragment 的转义，query 原样保留；所以这里只检查
    // rawPath + hash，不检查 search（否则 ?x=100% 会误判为 download）。
    const rawPath = rawPathFromURL(rawUrl);
    if (/%(?![0-9A-Fa-f]{2})/.test(rawPath + parsed.hash)) {
      return 'download';
    }
    // 与 Go url.Parse 一致：对原始路径做一次百分号解码（得到 decoded Path）
    let path;
    try {
      path = decodeURIComponent(rawPath);
    } catch (e) {
      return 'download';
    }
    // 去掉末尾的 /（只去掉一个，对应 strings.TrimSuffix(path, "/")）
    const trimmed = path.replace(/\/$/, '');
    if (!trimmed) {
      // 纯 / 路径
      let name = 'index.html';
      if (parsed.search) name += parsed.search;
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
    // 查询参数附加在文件名后
    if (parsed.search) name += parsed.search;
    return name || 'download';
  }

  // filepathSafe 清理文件名中的路径分隔符，防止路径穿越。
  // 与 Go pkg/cloudfilename.Safe 一致：替换 \ / ? : < > | " * 与 NUL 为 _，
  // 去除首尾空白与点，清理后为空时返回 "download"。
  function filepathSafe(name) {
    name = name.replace(/\x00/g, '');
    name = name.replace(/[\\/:<>|"*?]/g, '_');
    name = name.replace(/^[ .]+|[ .]+$/g, '');
    if (!name) return 'download';
    return name;
  }

  return { genDefaultFilename: genDefaultFilename, filepathSafe: filepathSafe };
});
