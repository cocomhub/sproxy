// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package secrets 是 secret 文件管理封装卷（设计 docs/designs/2026-10-01-secret-volume.md
// §6.1）：底层指向任意卷的 `secrets/` 目录（可以是 local/baidupcs/s3/webdav，
// 也可以是 secret_data 加密卷——嵌套封装）。显式卷寻址 `secrets://<卷名>/<name>`。
// 管理 API：创建（随机 32B hex 密钥）/ 列 / 读 / 删。
//
// **默认卷是 per-owner 的**（用户裁定 2026-10-15）：`DefaultVolumeName` 在每个 owner 的租户根下
// 各有一个独立桶，由 `/api/secrets` 按**请求 owner 上下文**解析（见 pkg/server），免声明、
// 首次使用自动派生。本包只提供按 FS 视图操作的 Manager，不含 owner 概念。
//
// secrets 卷只存普通文件（secret 文件本身不加密）；本地盘创建时权限 0600
// （ssh 密钥式），外部盘（baidupcs/s3）无权限要求（能拿到 = 有权限）。
package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// DefaultVolumeName 是 **per-owner 默认 secrets 卷名**（用户裁定 2026-10-15）：每个 owner 各有一个
// 同名默认 secrets 卷，落该 owner 租户根的 **meta 桶**内 `meta/secrets/` 目录
// （`<storage_root>/<owner>/meta/secrets/`）——同名 per-owner 隔离、免声明、owner 首次使用时自动
// 派生；在 meta 桶内使密钥字节不计入该 owner 的用户用量/文件数（见 pkg/server/secrets_api.go）。
//
// 它**不是** registry 里的已装配卷：寻址由**请求 owner 上下文**决定（服务端
// `Handlers.secretsManagerFor`），故 `secrets://default-secrets/<name>` 一类 URL 在**无 owner
// 上下文**的服务级声明（如 secretdata 的 `extra.secret_url`，装配期解析）中不可寻址——服务级
// 声明须引用**显式命名**的 secrets 卷（跨 owner 共享库）。该常量是「默认卷名」的唯一事实源
// （服务端管理面与本包文案共用），避免字面量散落。
const DefaultVolumeName = "default-secrets"

// ErrNotFound 是读取/删除不存在的 secret 的哨兵错误。
var ErrNotFound = errors.New("secrets: secret 不存在")

// ErrInvalidSecretName 是非法 secret 名（空/含路径分隔符/`.`/`..`）的哨兵错误。
// HTTP 层据此分类 400（客户端输入错误）vs 500（服务端 IO 故障）——避免脆弱文案匹配。
var ErrInvalidSecretName = errors.New("secrets: 非法 secret 名")

// ErrInvalidSecretValue 是非法 secret 值（非 64 位小写 hex）的哨兵错误。
var ErrInvalidSecretValue = errors.New("secrets: 非法 secret 值（须为 64 位小写 hex）")

// Manager 是 secrets 卷管理句柄：底层任意 sync.FS + 卷名寻址。
// 线程安全：底层 FS 自身保证（LocalFS os 调用 / 外部后端并发安全）。
type Manager struct {
	fs    syncpkg.FS
	name  string
	local bool
	// chmod 是可选的权限收紧回调（见 WithLocalChmod）：nil = 回落 `*LocalFS` 类型断言路径。
	chmod func(name string) error
}

// NewManager 构造 secrets 管理句柄。
// fs：底层卷的 secrets/ 目录视图（sync.FS）；name：卷名（寻址前缀）；
// local：底层是否为本地盘（决定 0600 权限语义）。
func NewManager(fs syncpkg.FS, name string, local bool, opts ...ManagerOption) *Manager {
	m := &Manager{fs: fs, name: name, local: local}
	for _, o := range opts {
		if o != nil {
			o(m)
		}
	}
	return m
}

// ManagerOption 配置 Manager 的可选装配项。
type ManagerOption func(*Manager)

// WithLocalChmod 注入 0600 权限收紧回调（按 secret 名）。为什么需要：权限收紧此前靠对 FS 做
// `*syncpkg.LocalFS` 类型断言——而经 `storage.Root` 适配的视图（`server.NewRootSecretsFS`，
// 加密/封印语义必需）不是 LocalFS，断言会**静默跳过**（本地加固消失）。装配层知道卷根，可给出
// 「按名收紧」的等价实现。未注入时保持既有 `*LocalFS` 断言路径（零回归）。
func WithLocalChmod(fn func(name string) error) ManagerOption {
	return func(m *Manager) { m.chmod = fn }
}

// Name 返回卷名（寻址 `secrets://<name>/` 前缀）。
func (m *Manager) Name() string { return m.name }

// validSecretName 校验 secret 名：非空、不含路径分隔符 `/` `\`、且不为 `.`/`..`
// （M-7 修复：`.`, `..` 会经底层 Stat 命中上层目录条目——Stat("..") 可命中父目录，
// Exists/SelectDefault 可能选中它随后 Read 失败；显式拒绝避免沿父引用越界）。
// 名字含前缀空格/后缀空格由调用方先 TrimSpace（路径语义收紧）。
func validSecretName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	return true
}

// Create 创建（或覆盖）一个 secret：生成随机 32B hex 密钥（64 字符）写入
// `secrets/<name>`（本地 0600）。返回生成的密钥字节。
func (m *Manager) Create(ctx context.Context, name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return nil, fmt.Errorf("%w %q（不能为空、不能含路径分隔符、不能为 . 或 ..）", ErrInvalidSecretName, name)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("secrets: 随机密钥生成失败: %w", err)
	}
	key := []byte(hex.EncodeToString(buf))
	if err := m.writeSecret(ctx, name, key); err != nil {
		return nil, err
	}
	return key, nil
}

// writeSecret 把 secret 密钥字节写入 `secrets/<name>`（本地 0600）。Create 与
// CreateFromPassphrase 共用落盘路径（IO 逻辑单一实现，任一改权限语义两者一致）。
func (m *Manager) writeSecret(ctx context.Context, name string, key []byte) error {
	if err := m.fs.WriteFile(ctx, name, bytes.NewReader(key), int64(len(key)), 0); err != nil {
		return fmt.Errorf("secrets: 写入 %q 失败: %w", name, err)
	}
	if m.local {
		// 本地盘权限 0600（ssh 密钥式）：优先用装配层注入的回调（经 Root 的视图无法类型断言）；
		// 否则底层为 LocalFS 时直接落盘后收紧权限。
		// 外部盘（baidupcs/s3）无权限要求——无法也无需 chmod。
		if err := m.chmodSecret(name); err != nil {
			return fmt.Errorf("secrets: 本地 secret %q 权限收紧失败: %w", name, err)
		}
	}
	return nil
}

// Import 校验并落盘一个**外部传入**的 secret 值（客户端本地生成/派生的 hex）到
// `secrets/<name>`（本地 0600）。用于上传链路（随机客户端生成 / 双口令 CLI 本地派生的
// 结果），服务端只做**格式校验 + 落盘**，不参与派生。
//
// 校验：`value` 须为 64 字符**小写** hex（32B 标准 secret 形态，与 Create 产物同构）；
// 任意非 hex / 长度不符 / 大写 → `ErrInvalidSecretValue`（fail-closed，防误把普通纯文本/
// 任意文件内容当 secret 写入卷）。覆盖写语义与 Create 一致（同名覆盖；secret 名唯一性
// 由调用方时序保证）。名非法 → `ErrInvalidSecretName`。
func (m *Manager) Import(ctx context.Context, name string, value []byte) ([]byte, error) {
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return nil, fmt.Errorf("%w %q", ErrInvalidSecretName, name)
	}
	if v := strings.TrimSpace(string(value)); !isSecretHex(v) {
		return nil, fmt.Errorf("%w（got %d 字节）", ErrInvalidSecretValue, len(value))
	}
	key := []byte(strings.TrimSpace(string(value)))
	if err := m.writeSecret(ctx, name, key); err != nil {
		return nil, err
	}
	return key, nil
}

// isSecretHex 判定值是否为 64 个小写 hex 字符（32B 字节的标准 secret 形态）。
func isSecretHex(v string) bool {
	if len(v) != hex.EncodedLen(32) {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// Read 读取 secret 内容（密钥字节）。
func (m *Manager) Read(ctx context.Context, name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return nil, fmt.Errorf("%w %q", ErrInvalidSecretName, name)
	}
	rc, err := m.fs.OpenRead(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("secrets: 打开 %q 失败: %w", name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("secrets: 读 %q 失败: %w", name, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secrets: secret %q 内容为空（损坏）", name)
	}
	return data, nil
}

// List 列出卷内全部 secret 名（排序）。
func (m *Manager) List(ctx context.Context) ([]string, error) {
	entries, err := m.fs.ListDir(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("secrets: 列目录失败: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out, nil
}

// Exists 探测 secret 是否存在。
func (m *Manager) Exists(ctx context.Context, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return false, fmt.Errorf("%w %q", ErrInvalidSecretName, name)
	}
	ent, err := m.fs.Stat(ctx, name)
	if err != nil {
		return false, err
	}
	return ent != nil, nil
}

// Remove 删除 secret 文件（校验名后经底层 FS 删除；不存在 fail-closed 返回错误，
// 不静默 no-op——删除不存在会掩盖调用方的名笔误）。
func (m *Manager) Remove(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return fmt.Errorf("%w %q", ErrInvalidSecretName, name)
	}
	if err := m.fs.Delete(ctx, name); err != nil {
		return fmt.Errorf("secrets: 删除 %q 失败: %w", name, err)
	}
	return nil
}

// SelectDefault 返回默认 secret 名：显式 defaultSecret 非空优先，否则全局默认
// （首个存在的 secret；无 secret 返回错误）。
func (m *Manager) SelectDefault(ctx context.Context, defaultSecret string) (string, error) {
	if strings.TrimSpace(defaultSecret) != "" {
		name := strings.TrimSpace(defaultSecret)
		if !validSecretName(name) {
			return "", fmt.Errorf("secrets: 非法默认 secret 名 %q", name)
		}
		return name, nil
	}
	names, err := m.List(ctx)
	if err != nil {
		return "", err
	}
	// 过滤非法条目（`..` 等）后再取首个——List 是目录枚举，可能含外部写入的异常名；
	// 直接 names[0] 会把异常名当默认 secret 返回，直到 Read 才报错（入口校验口径不一致）。
	valid := names[:0]
	for _, n := range names {
		if validSecretName(n) {
			valid = append(valid, n)
		}
	}
	if len(valid) == 0 {
		return "", fmt.Errorf("secrets: 卷 %q 无 secret 可用（先 Create）", m.name)
	}
	return valid[0], nil
}

// ---- owner 上下文（per-owner 默认桶寻址） ----

// ownerCtxKey 是 ctx 中「当前操作 owner」的私有键类型：**唯一读写入口**为 WithOwner /
// OwnerFromContext（值随 ctx 传递，无包级可变状态 ⇒ 天然并发安全，无需 seam 替换）。
type ownerCtxKey struct{}

// WithOwner 把「当前操作的 owner」放入 ctx。
//
// 为什么需要：默认 secrets 卷（`DefaultVolumeName`）是 **per-owner** 的桶，而 `secret_url` 的
// 服务级解析（cmd/sproxy.defaultSecretdataSecret）发生在**没有 HTTP 身份**的装配代码里。装配期
// （boot）确实没有 owner ⇒ 不可寻址（显式报错）；但**运行期**由某 owner 发起建卷（
// `POST /api/volumes/user`）时，请求上下文里有 owner ⇒ 该请求内构造的后端可解析到**它自己的**
// 默认桶（`secrets://default-secrets/<name>` 语义闭环）。
//
// 空 owner 原样返回（不改写 ctx，便于调用方无分支调用）。
func WithOwner(ctx context.Context, owner string) context.Context {
	if strings.TrimSpace(owner) == "" {
		return ctx
	}
	return context.WithValue(ctx, ownerCtxKey{}, owner)
}

// OwnerFromContext 返回 ctx 中的 owner（未设置 → 空串）。
func OwnerFromContext(ctx context.Context) string {
	s, _ := ctx.Value(ownerCtxKey{}).(string)
	return s
}

// IsDefaultVolumeRef 判定 `secrets://<authority>/<名>` 的 authority 是否指向 **per-owner 默认桶**：
// 空 / `default` / `DefaultVolumeName`。**唯一实现**：服务级解析（cmd/sproxy）与任何需要区分
// 「默认桶 vs 显式命名卷」的调用方共用，避免判定口径漂移。
func IsDefaultVolumeRef(urlStr string) bool {
	u, err := url.Parse(strings.TrimSpace(urlStr))
	if err != nil {
		return false
	}
	auth := u.Host
	if auth == "" {
		if before, _, ok := strings.Cut(u.Opaque, "/"); ok {
			auth = before
		} else {
			auth = u.Opaque
		}
	}
	switch strings.TrimSpace(auth) {
	case "", "default", DefaultVolumeName:
		return true
	}
	return false
}

// NameFromURL 解析 `secrets://<卷>/<name>`（兼容 Opaque 形态 `secrets://<name>`）里的 secret 名，
// 并做命名校验（非空、不含路径分隔符、非 `.`/`..`）。**唯一实现**：`backend.OpenURL` 与
// 服务级 per-owner 解析共用，避免两处解析口径漂移。
func NameFromURL(urlStr string) (string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("secrets: OpenURL 解析 %q 失败: %w", urlStr, err)
	}
	if !strings.EqualFold(u.Scheme, "secrets") {
		return "", fmt.Errorf("secrets: 不支持 scheme %q（需 secrets://）", u.Scheme)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if u.Opaque != "" && u.Host == "" {
		if _, after, ok := strings.Cut(u.Opaque, "/"); ok {
			name = after
		}
	}
	name = strings.TrimSpace(name)
	if !validSecretName(name) {
		return "", fmt.Errorf("%w %q", ErrInvalidSecretName, name)
	}
	return name, nil
}

// ---- ExternalBackend 适配 ----

// backend 是 secrets 卷的 registry.ExternalBackend 实现：持 Manager + sync.FS 视图。
type backend struct {
	mgr *Manager
	fs  syncpkg.FS
}

func (b *backend) FS() syncpkg.FS { return b.fs }
func (b *backend) Close() error   { return nil }

// IsLocalVolume 封装卷本地性自述（syncpkg.LocalVolume 能力接口，用户裁定 2026-10-05）：
// 封装卷必须实现并委派被封装的底层卷——secrets 卷底层为本地加密存储（LocalFS →
// 内部）；转存目标已被 NM6 拒绝（secrets 密钥卷非通用转存目标），委派保持一致语义。
func (b *backend) IsLocalVolume() bool {
	if lv, ok := b.fs.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

var _ registry.ExternalBackend = (*backend)(nil)
var _ registry.URLResolver = (*backend)(nil)

// OpenURL 解析 secrets://<卷>/<name> 并读取 secret 内容（RFC 3986）。
//
// 语义：authority（u.Host）= 卷名（dispatcher 已按卷名定位到本后端实例，此处不再
// 校验卷名）；path = secret 名（禁空、禁含 /，沿用 Manager 命名校验；解析在 NameFromURL
// ——唯一实现）。fail-closed：非法 URL / 空名 / 读取失败 → 明确错误。
func (b *backend) OpenURL(ctx context.Context, urlStr string) (io.ReadCloser, error) {
	name, err := NameFromURL(urlStr)
	if err != nil {
		return nil, err
	}
	data, err := b.mgr.Read(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("secrets: OpenURL 读 %q 失败: %w", name, err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// NewBackend 按卷描述构造 secrets 外部后端（V3 plugin）：
//   - v.Extra["target"]：底层卷名（必填；local 指本地默认卷 secrets/ 目录）；
//   - 底层寻址：targetFS 是**底层卷的 secrets/ 目录视图**——本地卷用
//     sync.NewLocalFS(<targetRoot>/secrets)；外部卷（baidupcs/s3/secretdata 等）
//     由装配层把底层后端 FS 的 `secrets/` 前缀视图传入（本函数不建前缀，由装配
//     按 target 类型决定——见 cmd/sproxy/secret_register.go）。
//
// **0600 加固回调自动接线**（2026-10-15 修复静默失效）：本地目标（`isLocalTarget`）且视图
// 自带 `ChmodSecret(name) error` 能力（如经 `storage.Root` 的 `server.NewRootSecretsFS`）时，
// 自动注入 `WithLocalChmod`——此前该回调只在装配层局部变量里**被丢弃**（`_ = mopts`），
// 于是经 Root 的视图（不是 `*LocalFS`）落到 `localChmod` 的类型断言 ⇒ **静默跳过权限收紧**。
//
// 说明：本函数签名设计为接收**已就绪的 secrets/ 视图 FS**（构造解耦，便于装配层
// 递归解析 secrets→secretdata 嵌套），故 Extra 只消费 target/root 元数据用于错误
// 文案与默认目录；实际 FS 由装配层经本函数的 fs 参数注入。
func NewBackend(ctx context.Context, v volume.Volume, fs syncpkg.FS) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("secrets backend: 卷 %q 类型 %q 不是外部 secrets 卷", v.Name, v.Type)
	}
	if fs == nil {
		return nil, fmt.Errorf("secrets backend: 卷 %q 底层 FS 未注入", v.Name)
	}
	local := isLocalTarget(v)
	var opts []ManagerOption
	if local {
		if c, ok := fs.(interface{ ChmodSecret(name string) error }); ok {
			opts = append(opts, WithLocalChmod(c.ChmodSecret))
		}
	}
	mgr := NewManager(fs, v.Name, local, opts...)
	return &backend{mgr: mgr, fs: fs}, nil
}

// isLocalTarget 判定底层目标是否为本地盘（决定 0600 权限语义）：Extra.target 为空或
// "local" 视为本地；baidupcs/s3/webdav/secretdata 等外部目标返回 false。
func isLocalTarget(v volume.Volume) bool {
	t, _ := v.Extra["target"].(string)
	return strings.TrimSpace(t) == "" || t == "local"
}

// localChmod 对本地 secret 文件收紧权限为 0600。
// 底层为 sync.LocalFS 时直接 chmod（见 local_chmod.go）；其它 FS（外部）不支持——忽略。
// chmodSecret 收紧 secret 权限：注入回调优先，否则回落 `*LocalFS` 断言（既有路径）。
func (m *Manager) chmodSecret(name string) error {
	if m.chmod != nil {
		return m.chmod(name)
	}
	return localChmod(m.fs, name)
}

func localChmod(fs syncpkg.FS, name string) error {
	lfs, ok := fs.(*syncpkg.LocalFS)
	if !ok {
		return nil // 外部盘无权限语义
	}
	return chmodPath(lfs, name)
}

// ManagerOfExternal 从 ExternalBackend 反取 secrets.Manager（装配层辅助）。
// 仅当 backend 是 secrets 卷适配器时返回非 nil；支持经透明包装（如 capacity.Backend
// 用 SecretsManagerAny() 泛型转发）后的反取。
type managerUnwrapper interface{ SecretsManager() *Manager }
type managerUnwrapperAny interface{ SecretsManagerAny() any }

func ManagerOfExternal(be registry.ExternalBackend) *Manager {
	if u, ok := be.(managerUnwrapper); ok {
		if m := u.SecretsManager(); m != nil {
			return m
		}
	}
	if u, ok := be.(managerUnwrapperAny); ok {
		if m, ok := u.SecretsManagerAny().(*Manager); ok {
			return m
		}
	}
	return nil
}

// SecretsManagerAny 暴露内部 Manager（供容量包装等透明层泛型转发）。
func (b *backend) SecretsManagerAny() any { return b.mgr }

// SecretsManager 暴露内部 Manager（供装配层反取）。
func (b *backend) SecretsManager() *Manager { return b.mgr }
