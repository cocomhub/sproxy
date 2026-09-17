// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/spf13/cobra"

	webdavgw "github.com/cocomhub/sproxy/pkg/gateway/webdav"
	"github.com/cocomhub/sproxy/pkg/remote"
)

const (
	flagDavListen  = "listen"
	defaultDavAddr = "127.0.0.1:8080"
)

// newCmdDav 创建 `sproxy dav` 子命令：本地 WebDAV 代理，把远端
// `remote://<node>/<vol>[/<path>]` 卷暴露为本地 WebDAV 端点。
//
// 用法：
//
//	sproxy dav [--listen 127.0.0.1:8080] remote://node/main
//	curl -X PUT http://127.0.0.1:8080/hello.txt -d world
//	rsync -av /local/ dav://127.0.0.1:8080/
//
// 凭据：复用主配置（--config）的 mesh.hub_url / mesh.access_key 等；
// hub 地址为空时指向本机 HTTP 面（newMeshHubClient 回落 newLocalSelfClient）。
func newCmdDav() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "dav [remote://node/vol[/path]]",
		Short: "本地 WebDAV 代理：把远端卷暴露为 WebDAV 端点",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := remote.ParseRef(args[0])
			if err != nil {
				return fmt.Errorf("解析 remote:// 句柄失败: %w", err)
			}

			cfg, err := buildServerConfig(cmd)
			if err != nil {
				return err
			}

			hubClient, err := newMeshHubClient(cfg, cfg.Mesh.AccessKey, cfg.Mesh.AccessKeySecret, cfg.Mesh.SkeyID)
			if err != nil {
				return fmt.Errorf("构造 mesh 客户端失败: %w", err)
			}
			dialer := remote.NewRelayDialer(hubClient, remote.ServiceName)

			handler, err := webdavgw.NewRemoteHandler(dialer, ref)
			if err != nil {
				return fmt.Errorf("构造 WebDAV handler 失败: %w", err)
			}
			defer handler.Close()

			slog.Info("WebDAV 代理启动", "listen", listen, "remote", ref.String())
			srv := &http.Server{Addr: listen, Handler: handler}
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("WebDAV 代理监听失败: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, flagDavListen, defaultDavAddr, "本地监听地址（默认仅回环）")
	return cmd
}
