// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"context"
	"errors"
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
//	读者 A newPC()                  → verbose / host-only / STUN / TURN / 静态凭据
//	读者 B ListenWithSignalerCtx()  → 信令超时（函数入口即读）、再次 newPC()
//	读者 C defaultConfig() 紧循环    → STUN / TURN / 静态凭据（高密度采样，同上均属真实读路径）
//
// 覆盖面如实说明：**写者覆盖 8 个全局，读者只覆盖其中 7 个**。
// 漏掉的是 rejectPrivateRemoteCandidates：它的唯一读点在 newPC 里被
// `if !hostOnlyEnabled() && !loopbackOnly` 门控，而本用例用 webrtctest.New(t) 开了
// loopback 候选收敛（loopbackOnly=true），该分支恒不进入、访问器永不被调（覆盖剖析实测 0.0%）。
// 不能靠"再补一个常规用例"覆盖：关掉 loopback 收敛会让 newPC 走非 loopback 的候选收集
// 路径，违反本项目「测试禁止触发 Windows 防火墙弹窗」的硬约束。
// 它的同步机制（atomic.Bool + 访问器）与已被本用例实际钉住的 useHostOnly、verbose 完全同构。
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

	// 写者：模拟 CLI 入口 / 测试 t.Cleanup 的 Set*/Reset* 序列，覆盖全部 8 个全局
	// （读者侧只覆盖 7 个，见测试头部说明）。
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

// TestConfigGlobals_SignalingTimeoutRace 是 signalingTimeout 的**聚焦**回归用例。
//
// 为什么不靠 TestConfigGlobals_ConcurrentSetAndRead 覆盖它：那个用例一轮里让写者翻 8 个
// 全局，race detector 的 shadow-memory 槽位会被高频写者反复覆盖，默认 GORACE 下
// 只**间歇**报到 signalingTimeout 这一对（本机 3 次默认配置运行命中 1 次；复审 4 次命中 1 次）
// ——而它恰是 CI 实际抓到的全局。
// 本用例把写面收窄到只写 SetSignalingTimeout/ResetSignalingTimeout，读面收窄到只走
// ListenWithSignalerCtx 的信令等待入口（它进入等待前必然执行 currentSignalingTimeout()），
// 让这一对在默认 GORACE 下**每次**都能被标记：未同步实现实测 5/5 红（每次 2 处，分别
// 命中写点 webrtc.go:160 SetSignalingTimeout 与 :165 ResetSignalingTimeout），
// 同步实现连跑 5 次稳定绿。
func TestConfigGlobals_SignalingTimeoutRace(t *testing.T) {
	// loopback 候选收敛：newPC 只绑 127.0.0.1，避免 Windows 弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	t.Cleanup(ResetSignalingTimeout)

	const iterations = 20

	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	// 写者：只翻 signalingTimeout，与生产里「CLI 收尾 / 测试 t.Cleanup 调
	// ResetSignalingTimeout」的写者形态一致。写到读者收工才停。
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stopWriter:
				return
			default:
			}
			SetSignalingTimeout(10 * time.Minute)
			ResetSignalingTimeout()
		}
	}()

	// 读者（主 goroutine）：只走信令等待入口。
	for i := 0; i < iterations; i++ {
		// ctx 尽量短：读点在函数入口，必然执行；短 ctx 让轮次更密、与写者的重叠窗口更大。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, err := ListenWithSignalerCtx(ctx, "race-peer", blockingSignaler{})
		cancel()
		// 断言必须证明「读者确实走到了读点」，不能只判 err != nil：
		// 读点在 context.WithTimeout(ctx, currentSignalingTimeout()) 那一行（位于 WaitOffer
		// 之前），但若 newPC 先失败（资源不足等），返回的 err 同样非 nil —— 只判非 nil 会让
		// 本用例退化成静默通过，前提失守。
		// 5ms 的父 ctx 先于信令超时（10min / 30s）到期 → blockingSignaler.WaitOffer 返回
		// context.DeadlineExceeded → 被 ListenWithSignalerCtx 包成 ErrNoIncomingConnection
		// （P1-11 的「空闲而非失败」哨兵，见 webrtc.go 的 wait offer 分支）。
		// 注意：该包装用 %w 只裹哨兵、不带 DeadlineExceeded，故这里断言哨兵而非 DeadlineExceeded。
		if !errors.Is(err, ErrNoIncomingConnection) {
			t.Fatalf("第 %d 轮：期望 ErrNoIncomingConnection（证明已走到信令等待读点），实际 %v", i, err)
		}
	}

	close(stopWriter)
	<-writerDone
}
