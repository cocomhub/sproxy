// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// ConfigURL 是官方 CLI 发布清单接口（OS/arch → 资产 URL）。
const ConfigURL = "https://config.mypikpak.com/config/v1/command_line?client=global"

// releaseAsset 是发布清单里的一个平台资产。
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// CliConfig 是 CLI 配置。
type CliConfig struct {
	// BinaryPath 是 pikpak 可执行文件路径；空 = 自动安装/查找。
	BinaryPath string
	// InstallDir 是自动安装目录；空 = 默认（~/.local/bin 或本地 bin）。
	InstallDir string
	// AutoInstall 是否允许自动下载官方 CLI（默认 true）。
	AutoInstall bool
	// ConfigURL 是发布清单 URL；空 = 官方默认。测试可注入本地 httptest。
	ConfigURL string
	// HTTPClient 可选注入（测试用）。
	HTTPClient *http.Client
	// CommandFactory 是命令构造器（测试可注入 fake，避免真实 CLI 依赖）。
	// nil = 默认 exec.CommandContext。实例级注入，无包级共享（测试并行安全）。
	CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Logger 日志。
	Logger *slog.Logger
}

// Cli 是官方 CLI 的包装：定位/安装/执行 pikpak 命令。
type Cli struct {
	bin    string
	client *http.Client
	log    *slog.Logger
	// commandFactory 是命令构造器（默认 exec.CommandContext；测试注入实例级 fake）。
	commandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// assetName 返回当前平台的 CLI 资产名（如 pikpak_windows_amd64.exe）。
func assetName() string {
	os := runtime.GOOS
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		arch = "amd64" // 未知架构回落
	}
	name := fmt.Sprintf("pikpak_%s_%s", os, arch)
	if os == "windows" {
		name += ".exe"
	}
	return name
}

// defaultInstallDir 返回默认安装目录：~/.local/bin（Unix）或 <cache>/pikpak-cli（Windows）。
func defaultInstallDir() string {
	if runtime.GOOS == "windows" {
		if cache, err := os.UserCacheDir(); err == nil {
			return filepath.Join(cache, "pikpak-cli")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

// NewCli 构造 CLI 包装。BinaryPath 为空时：
//  1. PATH 查找 pikpak（Unix）或同目录 pikpak.exe（Windows）；
//  2. 找不到且 AutoInstall 时下载官方 CLI 到 InstallDir。
func NewCli(cfg CliConfig) (*Cli, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: netutil.IsolatedTransport()}
	}
	bin := cfg.BinaryPath
	if bin == "" {
		bin = findInPath()
	}
	if bin == "" && cfg.AutoInstall {
		installDir := cfg.InstallDir
		if installDir == "" {
			installDir = defaultInstallDir()
		}
		cfgURL := cfg.ConfigURL
		if cfgURL == "" {
			cfgURL = ConfigURL
		}
		bin = installCLI(client, installDir, cfgURL, cfg.Logger)
	}
	if bin == "" {
		return nil, fmt.Errorf("pikpak CLI not found; install via install.sh or set binary_path")
	}
	factory := cfg.CommandFactory
	if factory == nil {
		factory = exec.CommandContext
	}
	return &Cli{bin: bin, client: client, log: cfg.Logger, commandFactory: factory}, nil
}

// findInPath 在 PATH 查找 pikpak 可执行文件。
func findInPath() string {
	name := "pikpak"
	if runtime.GOOS == "windows" {
		name = "pikpak.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// installCLI 下载并解压官方 CLI 到 installDir，返回可执行路径。
func installCLI(client *http.Client, dir, cfgURL string, log *slog.Logger) string {
	name := assetName()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warn("pikpak cli: mkdir install dir failed", "err", err, "dir", dir)
		return ""
	}
	dest := filepath.Join(dir, name)
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return dest // 已安装
	}
	// 从发布清单拿当前版本 URL
	cfg, err := fetchReleaseConfig(client, cfgURL)
	if err != nil {
		log.Warn("pikpak cli: fetch release config failed", "err", err)
		return ""
	}
	var url string
	for _, a := range cfg.Values.CommandLine.Assets {
		if a.Name == name {
			url = a.BrowserDownloadURL
			break
		}
	}
	if url == "" {
		log.Warn("pikpak cli: asset not found", "asset", name)
		return ""
	}
	if err := downloadFile(client, url, dest); err != nil {
		log.Warn("pikpak cli: download failed", "err", err, "url", url)
		return ""
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dest, 0o755)
	}
	log.Info("pikpak cli installed", "path", dest)
	return dest
}

// fetchReleaseConfig 拉取发布清单（版本 → 资产 URL）。
func fetchReleaseConfig(client *http.Client, url string) (*releaseConfig, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rc releaseConfig
	if err := json.NewDecoder(resp.Body).Decode(&rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

// releaseConfig 是发布清单 JSON 结构。
type releaseConfig struct {
	Values struct {
		CommandLine struct {
			Assets []releaseAsset `json:"assets"`
		} `json:"command_line"`
	} `json:"values"`
}

// downloadFile 下载 URL 到 dest（覆盖）。
func downloadFile(client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = ioCopy(out, resp.Body)
	return err
}

// run 执行 pikpak 命令（args...），返回 stdout。
func (c *Cli) run(ctx context.Context, args ...string) (string, error) {
	cmd := c.commandFactory(ctx, c.bin, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("pikpak %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// RunJSON 执行 pikpak 命令并解析 JSON 输出（-F json）。
func (c *Cli) RunJSON(ctx context.Context, out any, args ...string) error {
	full := append([]string{}, args...)
	full = append(full, "-F", "json")
	stdout, err := c.run(ctx, full...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		return fmt.Errorf("pikpak decode json: %w (out: %s)", err, truncate(stdout, 300))
	}
	return nil
}
