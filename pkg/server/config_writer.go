// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_writer.go 是「运行时用户卷 ↔ config 声明」的写回能力（C4，2026-10-06 用户裁决）：
//
//   - /api/volumes/user 创建卷成功后，把新卷追加到 config 文件 `volumes:` 段（重启后卷不丢）；
//   - 删除卷成功后，从 config 文件的 `volumes:` 段移除；
//   - config 写回失败 → 创建侧回滚（Set/store/links/配额一并回滚，保持一致性）；删除侧
//     **先写回 config 再删卷**（失败即终止删除，卷保留——I-4，杜绝「已删卷被 config 残留
//     复活」僵尸卷）。
//
// 实现（FileConfigWriter）用 gopkg.in/yaml.v3 的 **yaml.Node** 只改 `volumes:` 段
// （评审 I-3，2026-10-07：整 doc round-trip 曾毁掉 config 全部注释 + 键序重整；Node 级
// 局部改写保留其余键序、注释与标量形态）。原子写（临时文件 + fsync + rename），UTF-8 无
// BOM；权限保留（原文件权限或新建 0644，不因 CreateTemp 0600 静默收窄）。

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ConfigWriter 是「运行时用户卷写回 config 文件」的能力接口。实现由装配层注入
// （main 持有 config 路径）；nil = 未注入（无 config 文件语义的装配/测试），写回跳过。
type ConfigWriter interface {
	// AppendVolume 把用户卷追加到 config 文件 volumes 段（已存在同卷名 → 原地更新防重复）。
	AppendVolume(v UserVolume) error
	// RemoveVolume 从 config 文件 volumes 段移除指定卷（不存在 → no-op 成功）。
	RemoveVolume(name string) error
}

// FileConfigWriter 是 ConfigWriter 的 YAML 文件实现（gopkg.in/yaml.v3，Node 局部改写）。
// path 为 config 文件路径（main 的 cfgFile）。
type FileConfigWriter struct {
	path string
}

// NewFileConfigWriter 构造以 config 文件路径为目标的写回器。
func NewFileConfigWriter(path string) *FileConfigWriter {
	return &FileConfigWriter{path: path}
}

// AppendVolume 实现 ConfigWriter：只改 volumes 段（追加/原地更新卷条目），保留其余键序
// 与注释（评审 I-3）。
func (w *FileConfigWriter) AppendVolume(v UserVolume) error {
	if w == nil || w.path == "" {
		return nil
	}
	root, err := w.loadNode()
	if err != nil {
		return err
	}
	seq, ok, err := findVolumesSeq(root)
	if err != nil {
		return err
	}
	if !ok {
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		mapping := rootMapping(root)
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "volumes"}, seq)
	}
	entry, err := volumeEntryNode(v)
	if err != nil {
		return err
	}
	for i, item := range seq.Content {
		if nameOfVolumeEntry(item) == v.Name {
			seq.Content[i] = entry // 原地替换（保持条目位置），不重复。
			return w.saveNode(root)
		}
	}
	seq.Content = append(seq.Content, entry)
	return w.saveNode(root)
}

// RemoveVolume 实现 ConfigWriter：只移除 volumes 段中 name 匹配的条目（无 volumes 段 /
// 不存在 → no-op 成功，不新增空段）。
func (w *FileConfigWriter) RemoveVolume(name string) error {
	if w == nil || w.path == "" {
		return nil
	}
	root, err := w.loadNode()
	if err != nil {
		return err
	}
	seq, ok, err := findVolumesSeq(root)
	if err != nil || !ok {
		return err
	}
	out := seq.Content[:0]
	removed := false
	for _, item := range seq.Content {
		if nameOfVolumeEntry(item) == name {
			removed = true
			continue
		}
		out = append(out, item)
	}
	if !removed {
		return nil // 不存在 → no-op（卷名笔误/已移除）。
	}
	seq.Content = out
	return w.saveNode(root)
}

// volumeEntryNode 把 UserVolume 映射为 config `volumes:` 段条目 mapping 节点
// （只写非空字段；capacity 写为 vol_capacity 数值节点）。
func volumeEntryNode(v UserVolume) (*yaml.Node, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	put := func(k string, val any) error {
		vn, err := anyToNode(val)
		if err != nil {
			return err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, vn)
		return nil
	}
	if err := put("name", v.Name); err != nil {
		return nil, err
	}
	if err := put("type", v.Type); err != nil {
		return nil, err
	}
	if v.Capacity > 0 {
		if err := put("vol_capacity", v.Capacity); err != nil {
			return nil, err
		}
	}
	if len(v.Extra) > 0 {
		if err := put("extra", v.Extra); err != nil {
			return nil, err
		}
	}
	// C4 CRITICAL（2026-10-07 真实浏览器复测）：运行时建卷（POST /api/volumes/user）注入
	// owner-only ACL（Mode=Allow + 单 owner）使键空间为独享 `user/<rel>`；若写回不落 ACL，
	// 重启后 config 声明卷 ACL 零值（parseVolumeACL 归 deny）→ Shared()==true → 键空间翻转
	// 成 `<owner>/user/<rel>` → 旧数据不可见。故有 Owner 的 UserVolume 必须补写 owner-only
	// ACL 段（对齐 config 卷 ACL 结构 VolumeACLConfig：mode allow + owners [<owner>]）。
	// Owner 空（config 声明卷无 owner）→ 不写 acl 段（保持现状，零回归）。
	if v.Owner != "" {
		if err := put("acl", VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{v.Owner}}); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// nameOfVolumeEntry 返回 volumes 序列条目的 name 字段（非 mapping/无 name → ""）。
func nameOfVolumeEntry(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "name" {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// anyToNode 把任意标量/结构值编码为 yaml 节点（yaml.Marshal → Unmarshal 进 Node，
// 取文档首内容节点）。
func anyToNode(val any) (*yaml.Node, error) {
	b, err := yaml.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("config 写回：编码 %T 失败: %w", val, err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal(b, &n); err != nil {
		return nil, fmt.Errorf("config 写回：解析编码结果失败: %w", err)
	}
	if len(n.Content) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: "null"}, nil
	}
	return n.Content[0], nil
}

// loadNode 读 config 文件为 yaml.Node（DocumentNode；不存在 → 空 mapping 文档，新建写回）。
func (w *FileConfigWriter) loadNode() (*yaml.Node, error) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
		}
		return nil, fmt.Errorf("config 写回：读取 %s 失败: %w", w.path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("config 写回：解析 %s 失败: %w", w.path, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
	}
	return &root, nil
}

// findVolumesSeq 在文档 mapping 下定位 `volumes:` 序列节点。无 mapping / 无 volumes 键 →
// (nil,false,nil)；volumes 键非序列 → 错误（config 非法 fail-closed）。
func findVolumesSeq(root *yaml.Node) (*yaml.Node, bool, error) {
	m := rootMapping(root)
	if m == nil {
		return nil, false, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "volumes" {
			continue
		}
		seq := m.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			return nil, false, fmt.Errorf("config 写回：volumes 键应为列表（got %v）", seq.Kind)
		}
		return seq, true, nil
	}
	return nil, false, nil
}

// rootMapping 返回文档的顶层 mapping 节点（DocumentNode → 首内容；非文档/空 → nil）。
func rootMapping(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		return root.Content[0]
	}
	if root.Kind == yaml.MappingNode {
		return root
	}
	return nil
}

// saveNode 原子写回（临时文件 + fsync + rename），UTF-8 无 BOM；保留原 config 权限
// （CreateTemp 默认 0600 会把运维 config 静默收窄——原文件存在则继承其权限，新建用 0644）。
func (w *FileConfigWriter) saveNode(root *yaml.Node) error {
	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("config 写回：序列化失败: %w", err)
	}
	dir := filepath.Dir(w.path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return fmt.Errorf("config 写回：创建目录 %s 失败: %w", dir, mkErr)
	}
	tmp, err := os.CreateTemp(dir, "*.sproxy-config.tmp")
	if err != nil {
		return fmt.Errorf("config 写回：创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if fi, serr := os.Stat(w.path); serr == nil {
		_ = tmp.Chmod(fi.Mode().Perm())
	} else {
		_ = tmp.Chmod(0o644)
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config 写回：写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config 写回：fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config 写回：关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, w.path); err != nil {
		return fmt.Errorf("config 写回：原子重命名失败: %w", err)
	}
	return nil
}
