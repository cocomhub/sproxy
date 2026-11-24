// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 云任务 TSV 解析（导出格式回环：id/status/filename/total_size/url/updated_at）。
'use strict';

(function () {
  var HEAD = ['id', 'status', 'filename', 'total_size', 'url', 'updated_at'];

  /** 解析 TSV：识别表头（否则按导出列序）；忽略空行与注释行（#）。 */
  function parseTSV(text) {
    var lines = String(text == null ? '' : text).split(/\r?\n/);
    var rows = [];
    var cols = HEAD.slice();
    var first = true;
    lines.forEach(function (line) {
      if (!line || line.charAt(0) === '#') { return; }
      var cells = line.split('\t');
      if (first) {
        first = false;
        if (cells[0] === 'id') { cols = cells.map(function (c) { return c.trim(); }); return; }
      }
      if (!cells.some(function (c) { return c.trim(); })) { return; }
      var obj = {};
      cols.forEach(function (key, i) { obj[key] = (cells[i] || '').trim(); });
      rows.push(obj);
    });
    return rows;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { parseTSV: parseTSV, HEAD: HEAD };
  }
  if (typeof window !== 'undefined') { window.v2cloudOps = { parseTSV: parseTSV, HEAD: HEAD }; }
})();
