/* SPDX-License-Identifier: Apache-2.0 */
/*
 * qrcode.js —— sproxyQR 包装层：把 vendored qrcode-generator（MIT，见
 * vendor/qrcode-generator.js 版权头）包装成 app 层可用的纯函数。
 *
 * 产物 UMD：浏览器全局 sproxyQR，Node module.exports。浏览器加载序要求
 * vendor/qrcode-generator.js 先于本文件（全局 root.qrcode）。
 *
 * 提供的 API（纯函数，不碰 DOM）：
 *   renderToMatrix(text) → Array<Uint8Array>  值 0/1，[row][col]，1=暗点
 *   renderToSVG(text)    → string             内联 <svg> 标记（深色模块）
 *   createQR(text)       → {getModuleCount, isDark}  透传 qrcode 实例（诊断用）
 *
 * 编码参数：qrcode(0, 'L') = 自动版本（1-40，按内容长度）+ ECC L。所有文本
 * 按 UTF-8 处理（vendored 库 addData 内部 utf8 编码）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('./vendor/qrcode-generator.js'));
  } else {
    root.sproxyQR = factory(root.qrcode);
  }
})(typeof self !== 'undefined' ? self : this, function (qrcode) {
  'use strict';

  if (!qrcode) throw new Error('sproxyQR: 缺少 qrcode-generator（先加载 vendor/qrcode-generator.js）');

  // 生成 qrcode 实例（typeNumber=0 自动版本，ECC L）。
  function createQR(text) {
    if (typeof text !== 'string') throw new TypeError('sproxyQR: text 必须为字符串');
    const qr = qrcode(0, 'L');
    qr.addData(text);
    qr.make();
    return qr;
  }

  // text → 模块矩阵 Array<Uint8Array>（值 0/1；[row][col]，1=暗点）。
  function renderToMatrix(text) {
    const qr = createQR(text);
    const n = qr.getModuleCount();
    const out = new Array(n);
    for (let r = 0; r < n; r++) {
      const row = new Uint8Array(n);
      for (let c = 0; c < n; c++) row[c] = qr.isDark(r, c) ? 1 : 0;
      out[r] = row;
    }
    return out;
  }

  // 转义 SVG 属性/文本（仅基础：& < > "）。
  function esc(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  // text → 内联 SVG 字符串（1 模块=1 rect；含 viewBox 与白色背景，可缩放）。
  function renderToSVG(text) {
    const qr = createQR(text);
    const n = qr.getModuleCount();
    const rects = [];
    for (let r = 0; r < n; r++) {
      for (let c = 0; c < n; c++) {
        if (qr.isDark(r, c)) rects.push('<rect x="' + c + '" y="' + r + '" width="1" height="1"/>');
      }
    }
    return '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ' + n + ' ' + n + '" shape-rendering="crispEdges">' +
      '<rect width="' + n + '" height="' + n + '" fill="#ffffff"/>' +
      '<g fill="#000000">' + rects.join('') + '</g></svg>';
  }

  return { renderToMatrix, renderToSVG, createQR };
});
