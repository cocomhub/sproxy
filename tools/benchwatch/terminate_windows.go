// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobTree 用 Windows Job Object 收割整棵进程树。
//
// 为什么必须用 Job Object：`taskkill /T /F` 的树枚举存在竞态——实测 `sh -c "sleep N & wait"`
// 形态下，终止先把中间进程（sh）杀掉，孤儿（sleep）随即脱离树而逃逸（父已死 ⇒ 被重新挂载），
// taskkill 返回成功却**没真正收割整棵树**，且不会给出任何提示；开发机是 Windows，卡死会残留
// `*.test`/`go` 进程占文件与端口。Job Object 的 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 语义是：
// 该 Job 的**最后一个句柄关闭时，Job 内所有进程被强制终止**——后代默认继承该 Job（除非显式
// 用 `CREATE_BREAKAWAY_FROM_JOB` 逃脱），因此「被监视命令正常退出」也会顺带收割它留下的孤儿。
//
// 已知的 Start→Assign 竞态窗口（如实说明）：子进程启动到 `AssignProcessToJobObject` 之间，
// 若它立刻派生孙进程，孙进程可能先于 Job 建立而逃逸。本工具不采用 `CREATE_SUSPENDED` + Assign +
// `ResumeThread` 来消除该窗口（会显著增加复杂度；且窗口极短、被监视命令是编译/链接产物，启动后
// 需先加载运行时才会派生后代，实际无法利用）。该窗口的兜底由「正常退出后 Job 关闭即收割孤儿」
// 与 CI 的 runner 清理承担——与旧 `taskkill` 的「永远不可靠」相比是净改进。
type jobTree struct {
	job windows.Handle // 创建的 Job 句柄（KILL_ON_JOB_CLOSE；release 时 CloseHandle 触发收割）
}

// beforeStart 在 Windows 上无需启动前设置（Job 在 afterStart 建立即可）。
func (jobTree) beforeStart(*exec.Cmd) {}

// afterStart 创建带 KILL_ON_JOB_CLOSE 的 Job，并把子进程**立即**分配进去。
//
// 返回的 note 会写入 stderr（不得静默降级）：若 Job 创建/分配失败，后代将无法被一次性收割，
// 此时 taskkill /T /F 兜底仍会尽力（但不可靠），并在 terminate 时如实说明。
func (t *jobTree) afterStart(cmd *exec.Cmd) (string, error) {
	if cmd.Process == nil {
		return "", errors.New("afterStart: cmd.Process 为 nil（Start 未成功）")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return "benchwatch: 创建 Job Object 失败，降级为 taskkill /T /F（不可靠，后代可能残留）", err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if setErr := setKillOnJobClose(job, info); setErr != nil {
		_ = windows.CloseHandle(job)
		return "benchwatch: 设置 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 失败，降级为 taskkill /T /F（不可靠）", setErr
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return "benchwatch: OpenProcess 子进程失败，降级为 taskkill /T /F（不可靠）", err
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return "benchwatch: AssignProcessToJobObject 失败（降级为 taskkill /T /F，不可靠）", err
	}
	t.job = job
	return "", nil
}

// terminate 先用 TerminateJobObject 一次杀整棵；失败再退回 taskkill /T /F 兜底，并**如实记录**
// 实际走了哪条路径（不再静默降级）。宽限期 grace 是给进程自行退出的机会（Windows 无可移植的
// 「温和终止信号」，TerminateJobObject 本身即强制，故不做两段）。
func (t *jobTree) terminate(cmd *exec.Cmd, grace time.Duration) (used, note string, err error) {
	if cmd.Process == nil {
		return "", "", nil
	}
	if grace > 0 {
		time.Sleep(grace)
	}
	if t.job != 0 {
		if termErr := windows.TerminateJobObject(t.job, 1); termErr == nil {
			return "Job Object（TerminateJobObject 一次杀整棵）", "", nil
		} else {
			// Job 终止失败（例如句柄已被关闭）：如实记录，再走 taskkill 兜底。
			used = fmt.Sprintf("Job Object（TerminateJobObject 失败：%v，退回 taskkill）", termErr)
		}
	}
	// taskkill /T /F 兜底：仍可能因树枚举竞态残留孤儿（见文件头注释），故如实提示。
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run(); err == nil {
		return used + "→ taskkill /T /F", "", nil
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return used + "→ 直接 Kill（进程已自行退出）", "", nil
		}
		return used + "→ 直接 Kill 也失败", "Windows 降级：taskkill /T /F 与直接 Kill 均失败，后代进程可能残留", err
	}
	return used + "→ 直接 Kill（taskkill 不可用）", "Windows 降级：taskkill 不可用，已只终止直接子进程；其后代可能残留", nil
}

// release 关闭 Job 句柄：KILL_ON_JOB_CLOSE 语义 ⇒ 若被监视命令已退出而其后代仍在，此刻被收割。
func (t *jobTree) release() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}

// newTreeController 返回 Windows 的 Job Object 控制器。
func newTreeController() treeController { return &jobTree{} }

// setKillOnJobClose 配置 Job 的 KILL_ON_JOB_CLOSE 标志。
// 拆成单独函数仅为避免 afterStart 里的 `:=` 遮蔽外层 err（golangci-lint govet shadow）。
func setKillOnJobClose(job windows.Handle, info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}
