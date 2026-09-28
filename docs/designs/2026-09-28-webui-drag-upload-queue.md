<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# WebUI 拖拽上传放置区 + 上传进度所见即所得（roadmap 13.1 / V5）设计

> 日期：2026-09-28 ｜ 状态：头脑风暴（待评审） ｜ 范围：从「点击选择文件」升级为「全局拖拽上传」+ 可视化上传队列卡片
> 关联：[2026-09-28-webui-wysiwyg-feedback.md](./2026-09-28-webui-wysiwyg-feedback.md)（V6 反馈）、[2026-09-28-webui-responsive-layout.md](./2026-09-28-webui-responsive-layout.md)（V2）。

## 1. 背景与目标

### 1.1 现状（源码实证，index.html:32-33）

- 上传仅「点击选择文件」（`<input type="file" id="file-input" multiple>`）；
- 上传进度：`upload-progress-container` 有进度条，但**无上传队列卡片**（无文件名/大小/速度/剩余/暂停/取消列表）；
- 无**拖拽放置区**（`dragenter/dragover/drop` 未绑定）；
- 无「上传完成定位」——完成后列表刷新无明显提示去往文件。

### 1.2 目标

- **全局拖拽放置**：拖拽文件进浏览器窗口 → 高亮放置区 → drop 即上传（支持多文件/目录）；
- **上传队列卡片**：每文件一张卡（文件名/大小/进度条/速度/剩余/暂停/取消/完成状态）；
- **所见即所得**：队列卡片实时更新；全部完成 → toast + 自动刷新列表 + 定位到新文件；
- 断点续传复用既有分块会话（`/upload/init|chunk|complete` 已有）。

### 1.3 非目标

- 不做上传性能引擎改造（复用现有 upload.js 分块管道）；
- 不做文件夹整树递归（首版多文件 + 可后续）。

## 2. 组件与接口

### 2.1 纯逻辑层（node:test 直测，app-render.js 或新 upload-queue.js）

```js
// 批次渲染：文件列表 → 上传卡片 HTML 数组
buildUploadCardHtml({name, size, progress, speed, eta, status, canCancel, canPause})
// 增量更新：当前卡片 + 状态 → 新卡片（无全量重建）
mergeUploadCard(prevHtml, state) -> newHtml
// 聚合：多文件 → 总进度/总速度/总剩余
aggregateBatch(items) -> {progress, speed, eta}
```

### 2.2 交互层（app.js / upload.js）

```
拖拽：
  document dragenter/dragleave（计数防闪烁）/dragover(dropEffect='copy')/drop
  放置 → 高亮区消失 → 逐个入队
队列：
  upload-queue-container 渲染状态卡片
  每卡：暂停（既有 session 支持）/取消/重试
完成 → toast + refreshList() + scrollIntoView 新文件
```

## 3. 错误处理

- 拖拽非文件（目录/空）→ 提示；单文件超限 → 复用 413 转分块；
- 队列中某个失败 → 单卡变红 + 其余继续 + 汇总错误数；
- 拖出窗口专区消除高亮（dragleave 计数归零）。

## 4. 测试 + 变异点

| 用例 | 断言 | 变异 → 应红 |
|------|------|------------|
| buildUploadCards | 状态/进度/速度字段正确转义 | 不转义 → XSS 红 |
| mergeUploadCard | 只更新变动字段（无全量重建） | 全量 → 性能断言红 |
| aggregateBatch | 总进度/速度/剩余聚合正确 | 求和错 → 红 |
| Playwright 拖拽 | drop 触发上传、卡片出现 | 不绑 drop → e2e 红 |
| 完成定位 | 完成后列表刷新 + 定位新文件 | 无定位 → e2e 红 |

## 5. 片划分

- **P1**：拖拽放置 + 高亮区 + 上传播率卡片（含 node test + Playwright 拖拽）
- **P2**：暂停/取消/重试、多文件聚合进度、完成立位滚动、上传速度平滑

## 6. 风险与零回归

- 保留「点击选择文件」原路径（新增拖拽不破坏既有）；断点续传直接复用现有分块会话；
- 队列卡片全新插入（旧 `upload-progress-container` 兼容保留）；触屏无法拖拽 → 保留点击上传兜底；
- 与 V4 对话框、V6 反馈独立可并行。