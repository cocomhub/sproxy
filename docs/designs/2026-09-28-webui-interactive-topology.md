<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 可交互 Hub 拓扑浏览器（roadmap 13.2 / V1）设计

> 日期：2026-09-28 ｜ 状态：头脑风暴（待评审） ｜ 范围：Web UI Hub 拓扑从「固定静态小图」升级为「自适应 + 可交互」浏览器
> 关联：[2026-09-24-webui-topology.md](./2026-09-24-webui-topology.md)（首版拓扑，RTT 分档边色已落地——本设计是其交互/可读性增强）、
> roadmap 13.1-V1。

## 1. 背景与目标

### 1.1 现状（源码实证，web/static/app-render.js:162-197）

- `topologySvg(nodes)`：**固定 `<svg viewBox="0 0 640 220">` + `style="width:100%;height:auto"`**；
- 节点环形均匀铺在 **`radius = 90`** 一圈（`cx=320, cy=110`），节点 id 标签 **`font-size="9"`**；
- **零交互**：无滚轮缩放、无拖拽平移、无点击详情、无 hover tooltip、无数据多级；
- 节点数 **>200 直接降级为表格**（`return '<div class="empty-msg">…'`）；
- RTT 边色分档（`topologyEdgeColor`）、hub 中心圆、legend 图例已落地（保留复用）。

### 1.2 目标

- 大盘节点（几十/几百）也能看清、能操作；
- 可放大缩小（0.2×−4×，指针锚点）、拖拽平移、回到中心；
- 节点可识别：标签防重叠、hover/点击高亮、tooltip/侧栏详情；
- 全屏浏览 + 表格 ↔ 拓扑双视图切换（不再 200 上限硬降级）。

### 1.3 非目标

- 不做真·3D/panorama；不做平滑物理动画引擎（可用 CSS transform 过渡）；
- 不改服务端 /api/hub/nodes 数据结构（仅消费现有 nodes[].rtt_ms/quality/id）。

## 2. 组件与接口

### 2.1 纯布局函数（node:test 直测，app-render.js）

```js
// 返回拓扑布局 [{id, x, y, rtt, quality}]
layoutTopology(nodes) ->
  nodes.length <= 20: ring(cx,cy,r) 均分角度
  nodes.length > 20 : ring + 分簇（按 rtt/quality 组）+ 分层筒距
  radius = 60 + len * 2.2   // 自适应画布
  collisionFix(pts): 后移重叠标签（投影法 2 轮）
// 返回 viewBox 尺寸
topologyViewBox(nodes) -> {w, h}
```

### 2.2 交互渲染层（app.js 装配，负责 DOM/事件）

```
拓扑容器 ↓
  SVG <g>（transform: translate+scale，滚轮以指针为锚放大缩小）
    hub 中心圆 + nodes
  transform 状态 {tx, ty, scale}
  pointer 事件：down→move→pan；wheel→zoom；click→selectNode(id)
  hover → 高亮子图 + tooltip
  detail 面板（右侧，选中节点显示 id/RTT/quality/stale/度）
  toolbar：放大 / 缩小 / 复位 / 全屏 / 切换表格
```

### 2.3 数据流

`showHub()` fetch `/api/hub/nodes` → 现有 `hashSortableTable` 表格 + 新增 `renderTopology(nodes)` → `layoutTopology(纯)` → `renderSVG`（交互层监听，纯布局不碰 DOM）。

## 3. 错误处理

- 节点数为 0 → 空态（现有）。
- 布局 text 超限（>2000 节点）→ 提示 + 强制表格（不卡死）。
- fetch 失败 → 复用现有 toast + 保持表格幂等。
- 缩放到极值 → clamp（0.2x-4x，图片手指操作也安全）。

## 4. 测试 + 变异点

| 用例 | 断言 | 变异 → 应红 |
|------|------|------------|
| 纯 ring(≤20) | 节点坐标均匀分布、半径随 len 增长 | ring 公式改为固定半径 → 拥挤断言失败 |
| >20 分簇 | 仍不重叠（两轮碰撞容错） | 去掉 collision → 重叠红 |
| 自适应半径 | 60+len*2.2 | 恒 90 → 大列表红 |
| topologyViewport | 随节点数扩展 w/h | 固定 640×220 → 越界红 |
| 交互（Playwright） | wheel→缩放、drag→平移、click→详情、全屏切换 | 事件不绑 → e2e 红 |
| >200 切换 | 表格↔拓扑开关可见，拓扑不再硬降级 | 硬降级残留 → 红 |

## 5. 片划分

- **P1**：layoutTopology 纯函数 + renderSVG（可交互基础：zoom/pan/click/recenter）+ node test — （含「从环形可稍用力导向」演进，保持老调用拓扑SVG函数签名）
- **P2**：detail 面板、tooltip、hover 高亮、全屏、表格↔拓扑切换
- **P3**：图例增强、键盘导航、性能（>1000 降级阈值调优）

## 6. 风险与零回归

- 旧 `topologySvg`/`topologyEdgeColor` 保留（无剥依赖），桌面 WebUI 默认由静态 → 交互（渐进增强，无 feature 开关，旧行为视觉一致）。
- 新增纯布局函数全部 node:test；交互 e2e 走 web/e2e（真浏览器，R10/R18 门禁覆盖）。
- legend 样式/色阶保持与既有 `topologyEdgeColor` 一致，零视觉回归风险。