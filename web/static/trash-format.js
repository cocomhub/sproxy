// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// trash-format.js 回收站列表渲染纯函数（WebUI 回收站视图；用户裁定 2026-10-19 ③：各卷独立管理 +
// 汇总展示）。软删落**文件所在卷**的 trash ⇒ 汇总视图必须显示每条的卷，并支持按卷筛选与按卷清空。
// 纯函数可 node --test 单测。

'use strict';

// trashVolumeOptions 汇总视图的卷下拉选项（保持首次出现序，去重；不含「全部」项）。
function trashVolumeOptions(entries) {
  const seen = [];
  (entries || []).forEach(function (e) {
    const v = (e && e.volume) || '';
    if (v && seen.indexOf(v) < 0) seen.push(v);
  });
  return seen;
}

// trashFilterEntries 按卷筛选条目（volume 为空 ⇒ 全部卷）。
function trashFilterEntries(entries, volume) {
  const list = entries || [];
  if (!volume) return list;
  return list.filter(function (e) { return ((e && e.volume) || '') === volume; });
}

// trashTableHtml 渲染回收站：卷筛选 + 「清空该卷 / 清空全部卷」动作 + 条目表（卷 / 原路径 / 恢复）。
// volume 为当前筛选（空 = 全部卷）。
function trashTableHtml(entries, volume) {
  const all = entries || [];
  if (all.length === 0) {
    return '<div style="padding:12px;color:var(--text-muted);">回收站为空</div>';
  }
  const vols = trashVolumeOptions(all);
  const options = ['<option value="">全部卷</option>'].concat(vols.map(function (v) {
    return '<option value="' + v + '"' + (v === volume ? ' selected' : '') + '>' + v + '</option>';
  })).join('');
  const bar = '<div style="display:flex;gap:8px;align-items:center;padding:6px;flex-wrap:wrap;">' +
    '<span style="font-size:12px;color:var(--text-muted);">卷</span>' +
    '<select id="trash-volume-filter" style="font-size:12px;">' + options + '</select>' +
    '<button type="button" id="trash-empty-vol-btn" style="font-size:12px;cursor:pointer;">清空该卷</button>' +
    '<button type="button" id="trash-empty-all-btn" style="font-size:12px;cursor:pointer;">清空全部卷</button>' +
    '</div>';
  const rows = trashFilterEntries(all, volume);
  if (rows.length === 0) {
    return bar + '<div style="padding:12px;color:var(--text-muted);">该卷回收站为空</div>';
  }
  const cell = 'padding:6px;border-bottom:1px solid var(--border);font-size:12px;';
  const body = rows.map(function (e) {
    return '<tr>' +
      '<td style="' + cell + '">' + ((e && e.volume) || '') + '</td>' +
      '<td style="' + cell + '">' + ((e && e.name) || '') + '</td>' +
      '<td style="' + cell + '">' +
      '<button type="button" class="trash-restore-btn" data-trash-rel="' + ((e && e.trash_rel) || '') +
      '" style="font-size:12px;cursor:pointer;">恢复</button>' +
      '</td></tr>';
  }).join('');
  const th = 'text-align:left;padding:6px;font-size:12px;color:var(--text-muted);';
  return bar + '<table style="width:100%;border-collapse:collapse;">' +
    '<tr><th style="' + th + '">卷</th><th style="' + th + '">原路径</th><th style="' + th + '">操作</th></tr>' +
    body + '</table>';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    trashTableHtml: trashTableHtml,
    trashVolumeOptions: trashVolumeOptions,
    trashFilterEntries: trashFilterEntries,
  };
}
