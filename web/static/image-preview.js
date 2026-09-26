// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// image-preview.js WebUI 图片预览纯函数（B4：图片缩略图/预览）。
// 服务端 /download 支持 ?transform=thumb&width=N 按需缩略（原文件不动）；
// 浏览器侧先加载缩略图（省带宽），点击/放大再切原图。纯函数可 node --test。

'use strict';

// previewImageUrl(name, width) → 缩略图下载 URL（?transform=thumb&width=N）。
function previewImageUrl(name, width) {
  var w = width || 1600;
  return '/download?filename=' + encodeURIComponent(name) + '&transform=thumb&width=' + w;
}

// previewOriginalUrl(name) → 原图 URL（不带 transform）。
function previewOriginalUrl(name) {
  return '/download?filename=' + encodeURIComponent(name);
}

// isImageName(name) → 扩展名是否可缩略图片（服务端 transform 注册表支持集合）。
function isImageName(name) {
  if (!name || typeof name !== 'string') return false;
  var ext = name.split('.').pop().toLowerCase();
  return ['jpg', 'jpeg', 'png', 'gif', 'bmp', 'webp', 'svg'].indexOf(ext) !== -1;
}

// 导出（node --test 用）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = { previewImageUrl: previewImageUrl, previewOriginalUrl: previewOriginalUrl, isImageName: isImageName };
}
