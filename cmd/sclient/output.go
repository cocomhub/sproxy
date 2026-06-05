// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/client"
)

// printFileList 将 FileInfo 切片格式化为表格输出到指定 writer。
func printFileList(files []client.FileInfo, w io.Writer) {
	for _, f := range files {
		if f.IsDir {
			fmt.Fprintf(w, "[DIR]  %-50s\n", f.Name+"/")
		} else {
			checksumStr := f.Checksum
			if len(checksumStr) > 16 {
				checksumStr = checksumStr[:16]
			}
			if checksumStr == "" {
				checksumStr = "-"
			}
			fmt.Fprintf(w, "       %-50s  %10s  %s\n", f.Name, client.FormatByte(float64(f.Size)), checksumStr)
		}
	}
}
