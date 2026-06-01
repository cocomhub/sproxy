// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
)

var genkeyCmd = &cobra.Command{
	Use:   "genkey",
	Short: "生成 tunnel_key 密钥",
	Run: func(cmd *cobra.Command, args []string) {
		key, err := tunnel.GenerateKey()
		if err != nil {
			fmt.Printf("生成密钥失败: %v\n", err)
			return
		}
		fmt.Println(key)
	},
}
