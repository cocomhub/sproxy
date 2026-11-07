// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remotefs

// fixture_test.go 是本包的包内装配夹具：真实 storage.Root + 真实 files.Service +
// 真实 registry.Set（单本地卷 main），以及 Deps 的四个假实现（审计/配额/元数据/listener 装配态）。
//
// 与 pkg/server 的同名夹具的差别：这里**不经 RegisterRoutes**（那会形成包循环），
// 只构造本包真正依赖的那一层能力——这也正是本包被切出来的判据（依赖面 ≤8）。
//
// Meta 实现只覆盖**本地卷**分支（sidecar 键 meta.MetaPath("user/<rel>")）：外部卷的 meta
// 键空间属于装配层知识，其覆盖面留在 pkg/server 的 meta_remote_test.go。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

const (
	testReaderNodeA = "nodeA"
	testReaderOwner = "alice"
	testReaderRel   = "docs/hello.txt"
	testReaderBody  = "hello-remote-read\n"
	// testReaderFP 是跨节点面测试用的假指纹（通用占位，非真实凭据）。
	testReaderFP = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

	testDualOwner = testReaderOwner
	testDualVol   = "main"
)

// fakePeerFingerprint 是 Peer 的测试实现（伪造已认证对端指纹）。
type fakePeerFingerprint struct{ fp string }

func (f fakePeerFingerprint) PeerFingerprint() string { return f.fp }

// sha256hex 返回 b 的 SHA-256 十六进制（请求头 X-File-Checksum 的线上形态）。
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// auditRecorder 记录跨节点面审计：按装配层同一 shape 输出 slog JSON 行（断言读的是
// 结构化字段，而非内部结构）。
type auditRecorder struct {
	bytes.Buffer
	logger *slog.Logger
	denied []string
}

func newAuditRecorder() *auditRecorder {
	r := &auditRecorder{}
	r.logger = slog.New(slog.NewJSONHandler(&r.Buffer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return r
}

func (r *auditRecorder) Record(ctx context.Context, evt Event) {
	r.logger.InfoContext(ctx, "audit",
		"action", evt.Action, "actor", "", "mesh", evt.Mesh,
		"object_type", evt.ObjectType, "object", evt.Object,
		"result", evt.Result, "detail", evt.Detail,
	)
}

func (r *auditRecorder) RecordWriteDenied(reason, node string) {
	r.denied = append(r.denied, reason+"|"+node)
}

// fixture 是单本地卷（main）的跨节点面装配夹具。
type fixture struct {
	cfg   *config.Config
	set   *registry.Set
	audit *auditRecorder
	deps  Deps
}

// newFixture 由已校验的 cfg 装配夹具（cfg.Volumes[0] 为被测卷）。
func newFixture(t *testing.T, cfg *config.Config) *fixture {
	t.Helper()
	volCfg := cfg.Volumes[0]
	if err := os.MkdirAll(volCfg.Root, 0o755); err != nil {
		t.Fatalf("MkdirAll 卷根: %v", err)
	}
	root, err := storage.OpenRoot(volCfg.Root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tenants := storage.NewTenantCache(root)
	t.Cleanup(func() { _ = tenants.Close() })

	vols := make([]volume.Volume, 0, len(cfg.Volumes))
	roots := make(map[string]*storage.Root, len(cfg.Volumes))
	pools := make(map[string]*quota.Pool, len(cfg.Volumes))
	vols = append(vols, volumeFromConfig(volCfg))
	roots[volCfg.Name] = root
	pools[volCfg.Name] = quota.NewPool(0)
	set := registry.NewSet(vols, roots, nil, pools, volCfg.Name)

	router := fixtureRouter{Set: set, tenants: tenants}
	svc, err := files.New(tenants, files.WithVolumes(router), files.WithLogger(slog.Default))
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	audit := newAuditRecorder()
	return &fixture{
		cfg:   cfg,
		set:   set,
		audit: audit,
		deps: Deps{
			Files:   func() *files.Service { return svc },
			Volumes: set,
			Quota:   func(string) *quota.Scope { return nil },
			Audit:   audit,
			Meta:    fixtureMeta{tenants: tenants},
			Config:  cfg,
			Logger:  slog.New(slog.DiscardHandler),
			Runtime: fixtureRuntime{cfg: cfg, muxOpts: nil},
		},
	}
}

// fixtureRouter 把 registry.Set + 租户缓存适配为 files.VolumeRouter。
//
// 路由语义与 pkg/files 的单卷内建实现一致（显式卷==唯一卷、恒默认租户、无配额预留）——
// 夹具只需要一条本地卷通路，不复制多卷路由/配额语义。
type fixtureRouter struct {
	*registry.Set
	tenants *storage.TenantCache
}

func (r fixtureRouter) Volumes() files.VolumeSet { return r.Set }

func (r fixtureRouter) Tenant(volName, owner string) *storage.Tenant {
	if volName != "" && volName != r.Default().Name {
		return nil
	}
	return r.tenants.TenantFor(owner)
}

func (r fixtureRouter) Locate(owner, rel string) (files.FileLocation, bool) {
	tnt := r.Tenant("", owner)
	if tnt == nil || tnt.Root() == nil {
		return files.FileLocation{}, false
	}
	if _, err := tnt.Root().Stat(rel); err != nil {
		return files.FileLocation{}, false
	}
	return files.FileLocation{VolumeName: r.Default().Name, Tenant: tnt}, true
}

func (r fixtureRouter) Route(_ context.Context, owner, _ string, _ string, _ int64, _ string) (files.UploadRoute, error) {
	return files.UploadRoute{
		VolumeName: r.Default().Name,
		Tenant:     r.tenants.TenantFor(owner),
		Release:    func() {},
	}, nil
}

func (r fixtureRouter) RouteTarget(ctx context.Context, owner, rel, volName string, size int64) (files.UploadRoute, error) {
	return r.Route(ctx, owner, rel, volName, size, "")
}

// fixtureMeta 是本地卷口径的元数据/下载路径解析（见文件头说明）。
type fixtureMeta struct {
	tenants *storage.TenantCache
}

func (m fixtureMeta) RemoteDownloadPath(_ context.Context, owner, _ string, remotePath string) (files.DownloadPath, error) {
	rel0, err := pathguard.ValidateFilePath(remotePath)
	if err != nil {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusBadRequest, Message: "invalid filename"}
	}
	tnt := m.tenants.TenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: "not found"}
	}
	rel, ok := tnt.UserRel(rel0)
	if !ok {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusBadRequest, Message: "invalid path"}
	}
	return files.DownloadPath{Filename: remotePath, Tenant: tnt, Rel: rel}, nil
}

func (m fixtureMeta) ReadRemoteMeta(_ context.Context, owner, _ string, remotePath string) ([]byte, error) {
	notFound := &files.HTTPError{Status: http.StatusNotFound, Message: "not found"}
	rel0, err := pathguard.ValidateFilePath(remotePath)
	if err != nil {
		return nil, notFound
	}
	tnt := m.tenants.TenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, notFound
	}
	rel, ok := tnt.UserRel(rel0)
	if !ok {
		return nil, notFound
	}
	abs, ok := tnt.Root().Abs(meta.MetaPath(rel))
	if !ok {
		return nil, notFound
	}
	raw, rerr := os.ReadFile(abs)
	if rerr != nil {
		return nil, notFound
	}
	return raw, nil
}

// fixtureRuntime 是 listener 的装配态：身份来自 cfg.Hub.XferIdentityFile（与装配层同一优先级
// 的简化版），mux 选项固定为空，无集群出口凭证池。
type fixtureRuntime struct {
	cfg     *config.Config
	muxOpts []mux.Option
}

func (r fixtureRuntime) Identity() (*tunnel.Identity, error) {
	return tunnel.LoadOrCreateIdentity(r.IdentityPath())
}

func (r fixtureRuntime) IdentityPath() string {
	if r.cfg.Hub.XferIdentityFile != "" {
		return r.cfg.Hub.XferIdentityFile
	}
	return filepath.Join(os.TempDir(), "sproxy-test-identity.json")
}

func (r fixtureRuntime) MuxOptions() []mux.Option { return r.muxOpts }

func (r fixtureRuntime) CredentialSource() (CredentialSource, error) { return nil, nil }

// volumeFromConfig 把配置卷翻译为领域卷（夹具只覆盖本地卷 + ACL/mesh_readers 两项）。
func volumeFromConfig(v config.VolumeConfig) volume.Volume {
	vol := volume.Volume{Name: v.Name, RootDir: v.Root}
	vol.ACL.Mode = volume.Mode(v.ACL.Mode)
	if owners := v.ACL.Owners; len(owners) > 0 {
		vol.ACL.Owners = make(map[string]struct{}, len(owners))
		for _, o := range owners {
			vol.ACL.Owners[o] = struct{}{}
		}
	}
	for _, mr := range v.ACL.MeshReaders {
		vol.ACL.MeshReaders = append(vol.ACL.MeshReaders, volume.MeshReader{
			Node: mr.Node, Fingerprint: mr.Fingerprint, Owner: mr.Owner, Scope: mr.Scope,
		})
	}
	return vol
}

// remoteReadTestConfigACL 构造「单卷 main + 给定 ACL」的已校验 cfg（不写入任何文件）。
func remoteReadTestConfigACL(t *testing.T, acl *config.VolumeACLConfig) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Volumes = []config.VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: acl}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config 非法: %v", err)
	}
	return cfg
}

// writeRemoteUserFile 在卷根上写入 <owner>/user/<rel>（自动建中间目录）。
func writeRemoteUserFile(t *testing.T, cfg *config.Config, owner, rel, body string) {
	t.Helper()
	abs := filepath.Join(cfg.StorageRoot, owner, "user", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// remoteReadTestConfig 构造「单卷 + 一条 mesh_readers + 磁盘上 docs/hello.txt」的已校验 cfg。
func remoteReadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := remoteReadTestConfigACL(t, &config.VolumeACLConfig{
		Mode:   config.VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []config.VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
		}},
	})
	writeRemoteUserFile(t, cfg, testReaderOwner, testReaderRel, testReaderBody)
	return cfg
}

// remoteWriteTestConfig 构造「单卷 + 一条 scope 可配的 mesh_readers」已校验 cfg。
// mode=allow 且 owners=[alice]：让 owner 本身过卷 ACL（第二重约束的第一半）。
func remoteWriteTestConfig(t *testing.T, scope string) *config.Config {
	t.Helper()
	return remoteReadTestConfigACL(t, &config.VolumeACLConfig{
		Mode:   config.VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []config.VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner, Scope: scope,
		}},
	})
}

// newRemoteReadFixture 返回 (只读面 handler, cfg, 审计缓冲)。
func newRemoteReadFixture(t *testing.T, peerFP string) (http.Handler, *config.Config, *auditRecorder) {
	t.Helper()
	cfg := remoteReadTestConfig(t)
	f := newFixture(t, cfg)
	return NewReadHandler(f.deps, fakePeerFingerprint{fp: peerFP}, nil), cfg, f.audit
}
