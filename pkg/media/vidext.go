// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package media

import (
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// vidext.go —— **视频扩展名判定单一事实源**（2026-10-21 收敛）。
//
// 背景：此前视频扩展名判定散落在 5 处（shardseal.MediaKindOf / integrity/ext/video
// VideoChecker.Matches / pikpak hasVideoExt / pikpak downloader.isVideo / Web UI
// VIDEO_EXT），各自维护一份列表，扩展名口径漂移（如 m4v 曾漏列、wmv/rmvb 缺失）。
// 本文件提供**唯一**判定入口，其它消费方复用；新增/修正扩展名只改这里。
//
// 三个口径（语义不同，须按需选择）：
//   - `IsVideoName`：**是视频文件**（含可校验但浏览器不原生播的容器，如 rmvb/wmv/flv）——
//     integrity 校验器 / 转存过滤 / 下载器过滤使用；
//   - `IsPlayableVideoName`：**浏览器原生可播子集**（标准 <video> Range 播放）。**当前无生产
//     调用方**（Web UI `app-render.js` 自带 `VIDEO_EXT` 可播集，跨语言无法直接复用）——本函数与
//     `playableVideoExt` 是**预留 API**（供未来 Go 侧渲染/SSR 或客户端 SDK 复用）；Web UI 侧
//     口径以 `web/static/app-render.js` 的 `VIDEO_EXT` 为事实源（有 node 单测锁定）；
//   - `shardseal.MediaKindOf`：**容器族选型**（关键帧分块）——secretdata 写路径用，扩展名 → 容器族，
//     由注册表裁决解析器；不直接消费本文件的判定（语义不同：选型需容器族，不只要 bool）。
//
// 大小写：统一先 `strings.ToLower` 再匹配（与 integrity/pikpak/preview 等既有判断点一致），
// Windows/网盘上传的大写后缀（WMV/RMVB/.MKV）同命中。

// videoExt 是「是视频文件」扩展名表（含可 ffprobe 校验的所有常见容器，**唯一事实源**）。
// 与 Web UI `VIDEO_EXT`（浏览器可播子集）的关系见 IsPlayableVideoName 注释。
var videoExt = map[string]bool{
	".mp4": true, ".mov": true, ".m4v": true, // ISO-BMFF（go-mp4 / ffprobe）
	".mkv": true, ".webm": true, // EBML
	".ts": true, ".m2ts": true, ".mts": true, // MPEG-TS
	".avi": true, ".divx": true, // RIFF
	".wmv": true, ".asf": true, // ASF
	".rmvb": true, ".rm": true, // RealMedia
	".flv": true, ".f4v": true, // FLV
	".mpg": true, ".mpeg": true, ".vob": true, ".mod": true, ".dat": true, // MPEG-PS
	".ogv": true,               // Ogg
	".3gp": true, ".3g2": true, // 3GPP
}

// playableVideoExt 是**保守**的「浏览器原生可播」子集（H.264/VP8 系，各主流浏览器一致可播）。
// 与 videoExt 的关系：**真子集**——服务端可校验（ffprobe）但浏览器不原生播放的容器
// （rmvb/wmv/flv/avi/mpeg/ogv/3gp 等）不在此表。
//
// **与 Web UI `VIDEO_EXT` 的关系（如实说明，非漂移）**：Web UI 的 `VIDEO_EXT` 是**更宽的
// 「可尝试子集」**（额外含 avi/ogv/3gp/3g2/mpg/mpeg/vob/mod/dat——部分浏览器可播，不支持的
// 浏览器 `<video>` 报错但不崩页面）；本表是**保守可播**口径（预留 API，见文件头）。两者语义
// 不同且**有意不同**：Web UI 宁可多给入口让用户试，Go 侧预留 API 取保守集。
var playableVideoExt = map[string]bool{
	".mp4": true, ".m4v": true, ".webm": true, // H.264/VP8 原生
	".mkv": true, ".mov": true, ".ts": true, // 部分浏览器可播（Chrome/Edge）
}

// IsVideoName 判断文件名是否为视频（扩展名 ∈ videoExt，大小写不敏感）。
// 语义：**是视频文件**（含可校验但浏览器不原生播的容器）——integrity 校验器归属、
// 转存过滤、下载器过滤使用。
func IsVideoName(name string) bool {
	return extOf(name, videoExt)
}

// IsPlayableVideoName 判断文件名是否为浏览器可原生播放的视频（扩展名 ∈
// playableVideoExt，大小写不敏感）。语义：**标准 <video> 可播**——Web UI ▶ 播放入口
// 判定使用；不适用「是否视频文件」类过滤（rmvb/wmv 等不可播但仍是视频）。
func IsPlayableVideoName(name string) bool {
	return extOf(name, playableVideoExt)
}

// extOf 按扩展名查表（统一 ToLower；无扩展名/非字符串返回 false）。
func extOf(name string, table map[string]bool) bool {
	if name == "" {
		return false
	}
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 || dot == len(name)-1 {
		return false
	}
	return table[strings.ToLower(name[dot:])]
}

// MediaKindOfDelegate 是 shardseal.MediaKindOf 的**测试对齐**：断言本文件的 videoExt
// 表与 shardseal 容器族选型覆盖一致（全仓扩展名口径单一事实源 vs 选型表的差集校验）。
// 生产不调用（选型走 shardseal.MediaKindOf，返回容器族而非 bool）。
func MediaKindOfDelegate(name string) string { return shardseal.MediaKindOf(name) }
