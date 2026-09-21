// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"testing"
)

// fakeEventSink 记录收到的事件（测试断言）。
type fakeEventSink struct {
	events []sinkEvent
}

type sinkEvent struct {
	action, owner, rel string
	size               int64
}

func (f *fakeEventSink) OnFileEvent(action, owner, rel string, size int64) {
	f.events = append(f.events, sinkEvent{action: action, owner: owner, rel: rel, size: size})
}

// TestService_Upload_PublishesEvent 验证上传成功后领域层推送 upload 事件（rel 去 user/ 前缀）。
func TestService_Upload_PublishesEvent(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	sink := &fakeEventSink{}
	env.svc = env.withEventSink(sink)

	body := []byte("event-body")
	env.upload(t, "alice", "sub/file.txt", body, sha256Hex(body), 0)

	if len(sink.events) != 1 {
		t.Fatalf("应推送 1 条事件, got %d (%+v)", len(sink.events), sink.events)
	}
	ev := sink.events[0]
	if ev.action != "upload" || ev.owner != "alice" || ev.rel != "sub/file.txt" {
		t.Fatalf("事件内容不符: %+v", ev)
	}
	if ev.size != int64(len(body)) {
		t.Fatalf("事件 size=%d want %d", ev.size, len(body))
	}
}

// TestService_Mkdir_PublishesEvent 验证 mkdir 推送目录事件。
func TestService_Mkdir_PublishesEvent(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	sink := &fakeEventSink{}
	env.svc = env.withEventSink(sink)

	if _, err := env.svc.MakeDir("alice", "newdir"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if len(sink.events) != 1 || sink.events[0].action != "mkdir" {
		t.Fatalf("应推送 mkdir 事件, got %+v", sink.events)
	}
	if sink.events[0].rel != "newdir" {
		t.Fatalf("mkdir 事件 rel=%q want newdir", sink.events[0].rel)
	}
}
