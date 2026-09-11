// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// blockingSignaler 是永不投递 Offer/Answer 的 Signaler：让 ListenWithSignalerCtx
// 停在信令等待路径上（受 ctx 与信令超时共同约束），从而每轮都走到读信令超时那一行。
type blockingSignaler struct{}

func (blockingSignaler) SendOffer(string, string) error  { return nil }
func (blockingSignaler) SendAnswer(string, string) error { return nil }

func (blockingSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}

func (blockingSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}

// TestConfigGlobals_ConcurrentSetAndRead 是包级可变配置的数据竞争回归测试。
//
// 钉住的缺陷：这些配置由导出的 Set* 写入、由连接建立路径读取，而 Set* 的调用时机
// 不受限（CLI 入口会调、测试用 t.Cleanup 复位），写入完全可能与"上一轮遗留、仍在
// 运行"的连接 goroutine 的读重叠。CI 实测到过该竞争：某测试 t.Cleanup 调
// ResetSignalingTimeout（写帧在 testing.(*common).Cleanup.func1）与 mesh accept
// goroutine 里的 ListenWithSignalerCtx（读帧在 mesh.runWebRTCAcceptLoop）并发。
//
// 本测试让写者（Set*/Reset* 序列）在读者运行期间持续写、三个读者并发读。关键：
// 读者走的是**真实生产读路径**（newPC / ListenWithSignalerCtx / defaultConfig），
// 不是测试专用的访问器，所以任何"读裸全局变量"的未同步实现都会在 -race 下被标记；
// 同步化实现则稳定通过。
//
//	读者 A newPC()                  → verbose / host-only / 私网候选过滤 / STUN / TURN / 静态凭据
//	读者 B ListenWithSignalerCtx()  → 信令超时（函数入口即读）、再次 newPC()
//	读者 C defaultConfig() 紧循环    → STUN / TURN / 静态凭据（高密度采样，同上均属真实读路径）
func TestConfigGlobals_ConcurrentSetAndRead(t *testing.T) {
	// loopback 候选收敛：新建的 PeerConnection 只绑 127.0.0.1，避免 Windows 弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()

	// 复位全部全局，避免污染同包其他测试。
	t.Cleanup(func() {
		SetSTUNServers(nil)
		SetTURNServers(nil)
		SetTURNCredential("", "")
		ResetSignalingTimeout()
		SetHostOnly(false)
		SetRejectPrivateRemoteCandidates(false)
		SetVerbose(false)
	})

	const iterations = 30

	var wg sync.WaitGroup
	stopWriter := make(chan struct{})

	// 写者：模拟 CLI 入口 / 测试 t.Cleanup 的 Set*/Reset* 序列，覆盖全部 8 个全局。
	// 一直写到三个读者都收工才停：写者若先跑完，某个全局可能刚好错过竞争窗口
	// （读者 A 进 newPC 前还要先建 PeerConnection），导致漏检。
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stopWriter:
				return
			default:
			}
			SetSignalingTimeout(10 * time.Minute)
			ResetSignalingTimeout()
			SetHostOnly(i%2 == 0)
			SetRejectPrivateRemoteCandidates(i%2 == 0)
			SetVerbose(i%2 == 0)
			SetSTUNServers([]string{"stun:stun.example.com:3478"})
			SetSTUNServers(nil)
			SetTURNServers([]string{"turn:relay.example.com:3478"})
			SetTURNCredential("race-user", "race-pass")
			SetTURNServers(nil)
			SetTURNCredential("", "")
		}
	}()

	// 读者 A：真实建连路径（内部走 defaultConfig + 日志工厂 + 远程候选过滤）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			pc, _, err := newPC()
			if err != nil {
				t.Errorf("newPC(): %v", err)
				return
			}
			_ = pc.Close()
		}
	}()

	// 读者 B：真实信令等待路径（入口读信令超时后进入等待；用短 ctx 快速返回）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			_, err := ListenWithSignalerCtx(ctx, "race-peer", blockingSignaler{})
			cancel()
			if err == nil {
				t.Error("blockingSignaler 不应建立连接")
				return
			}
		}
	}()

	// 读者 C：在固定窗口内紧循环走 defaultConfig()——它同属真实生产读路径（newPC
	// 就调它），但不建 PeerConnection，采样密度比读者 A 高一到两个数量级，专门提高
	// STUN / TURN 列表与静态凭据这几个切片/字符串全局的竞争命中率（只靠 A 那一轮一条
	// 连接来采样，窗口太稀）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			_ = defaultConfig()
		}
	}()

	wg.Wait()
	close(stopWriter)
	<-writerDone
}
