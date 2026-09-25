// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/cocomhub/sproxy/pkg/client"
)

// MirrorOptions 是镜像/联邦配置生成的参数（CLI 从 flag 注入）。
type MirrorOptions struct {
	// TargetURL 是目标机地址（YAML 注释提示合并进目标配置）。
	TargetURL string
	// AccessKey / AccessKeySecret / AccessKeyID 是源机认可的 SproxySig 凭据
	// （写入 sync_remotes/federation 片段；为空时省略对应字段）。
	AccessKey       string
	AccessKeySecret string
	AccessKeyID     string
	// MirrorInterval 是 volumes[].mirror_interval（周期镜像间隔；0 = 省略）。
	MirrorInterval string
}

// GenerateMirrorConfig 由 manifest + 目标机卷列表生成 YAML 片段：
//
//	volumes[]（目标卷名 + mirror_to/mirror_targets 多卷冗余，源文件卷落到冗余目标）
//	sync_remotes[]（源机为 direct 同步远端，含 SproxySig 凭据）
//	federation.peers（源机为联邦对端 hub，含凭据）
//
// 返回可直接打印（--print）或落盘（--write）的 YAML 文本。
func GenerateMirrorConfig(m *Manifest, vols []client.VolumeInfo, opts MirrorOptions) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest 为空")
	}
	// 源机卷集合（manifest 文件条目中出现的卷；空 = auto/单卷视图）。
	srcVols := make(map[string]bool)
	for _, f := range m.Files {
		if f.Volume != "" {
			srcVols[f.Volume] = true
		}
	}
	// 目标卷列表（ACL 允许）。首卷为默认落盘卷；其余为冗余目标（mirror_to 候选）。
	var allowed []client.VolumeInfo
	for _, v := range vols {
		if v.Allowed {
			allowed = append(allowed, v)
		}
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].Name < allowed[j].Name })

	var buf bytes.Buffer
	buf.WriteString("# 迁移向导生成（sclient migrate mirror-config）——合并进目标机配置后重启生效\n")
	if opts.TargetURL != "" {
		buf.WriteString("# 目标机: " + opts.TargetURL + "\n")
	}

	// volumes 段：目标卷名 + 多卷冗余（mirror_to = 下一卷；无卷不生成）。
	if len(allowed) > 0 {
		buf.WriteString("volumes:\n")
		for i, v := range allowed {
			line := fmt.Sprintf("  - name: %s\n", v.Name)
			if len(allowed) > 1 && i < len(allowed)-1 {
				line += fmt.Sprintf("    mirror_to: %s\n", allowed[i+1].Name)
			}
			buf.WriteString(line)
		}
	}

	// sync_remotes 段：源机为 direct 远端（manifest 无源机 URL 时不生成）。
	if m.Server.URL != "" {
		buf.WriteString("sync_remotes:\n")
		line := fmt.Sprintf("  - name: src\n    kind: direct\n    url: %s\n", m.Server.URL)
		if opts.AccessKey != "" {
			line += fmt.Sprintf("    access_key: %s\n", opts.AccessKey)
		}
		if opts.AccessKeySecret != "" {
			line += fmt.Sprintf("    access_key_secret: %s\n", opts.AccessKeySecret)
		}
		if opts.AccessKeyID != "" {
			line += fmt.Sprintf("    access_key_id: %s\n", opts.AccessKeyID)
		}
		buf.WriteString(line)
	}

	// federation 段：源机为联邦对端 hub。
	if m.Server.URL != "" {
		buf.WriteString("federation:\n  enabled: true\n  peers:\n")
		line := fmt.Sprintf("    - id: src\n      url: %s\n", m.Server.URL)
		if opts.AccessKey != "" {
			line += fmt.Sprintf("      access_key: %s\n", opts.AccessKey)
		}
		if opts.AccessKeySecret != "" {
			line += fmt.Sprintf("      access_key_secret: %s\n", opts.AccessKeySecret)
		}
		if opts.AccessKeyID != "" {
			line += fmt.Sprintf("      access_key_id: %s\n", opts.AccessKeyID)
		}
		buf.WriteString(line)
	}

	// 汇总注释（多卷/联邦语义说明）。
	if len(srcVols) > 0 {
		var names []string
		for v := range srcVols {
			names = append(names, v)
		}
		sort.Strings(names)
		buf.WriteString("# 源机卷: " + strings.Join(names, ", ") + "\n")
	}

	// 单卷目标机：无冗余可配（零回归；不生成空 volumes 段）。
	return buf.Bytes(), nil
}

// MirrorConfigYAML 是 GenerateMirrorConfig 输出的可反解形状（供测试/调试断言）。
// 生产不用——仅测试辅助。
type MirrorConfigYAML struct {
	Volumes []struct {
		Name     string `yaml:"name"`
		MirrorTo string `yaml:"mirror_to,omitempty"`
	} `yaml:"volumes"`
	SyncRemotes []struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
		URL  string `yaml:"url"`
	} `yaml:"sync_remotes"`
	Federation struct {
		Enabled bool `yaml:"enabled"`
		Peers   []struct {
			ID  string `yaml:"id"`
			URL string `yaml:"url"`
		} `yaml:"peers"`
	} `yaml:"federation"`
}
