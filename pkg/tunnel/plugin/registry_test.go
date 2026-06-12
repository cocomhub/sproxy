// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package plugin_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
)

type testInterface interface {
	Execute() string
}

type builtinImpl struct{}

func (b builtinImpl) Execute() string { return "builtin" }

type externalImpl struct{ value string }

func (e externalImpl) Execute() string { return e.value }

func TestRegistryActiveReturnsBuiltinWhenNoPlugins(t *testing.T) {
	r := plugin.New[testInterface]("test", builtinImpl{})
	active := r.Active()
	if active.Execute() != "builtin" {
		t.Fatalf("expected 'builtin', got %q", active.Execute())
	}
}

func TestRegistryActiveReturnsHighestPriority(t *testing.T) {
	r := plugin.New[testInterface]("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "low", Instance: externalImpl{"low"}, Priority: 1})
	r.Register(plugin.Plugin[testInterface]{Name: "high", Instance: externalImpl{"high"}, Priority: 10})
	active := r.Active()
	if active.Execute() != "high" {
		t.Fatalf("expected 'high', got %q", active.Execute())
	}
}

func TestRegistryGet(t *testing.T) {
	r := plugin.New[testInterface]("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "foo", Instance: externalImpl{"bar"}, Priority: 5})
	inst, found := r.Get("foo")
	if !found {
		t.Fatal("expected to find 'foo'")
	}
	if inst.Execute() != "bar" {
		t.Fatalf("expected 'bar', got %q", inst.Execute())
	}

	_, found = r.Get("nonexistent")
	if found {
		t.Fatal("expected not to find nonexistent")
	}
}

func TestRegistryNames(t *testing.T) {
	r := plugin.New[testInterface]("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "a", Instance: externalImpl{"a"}, Priority: 1})
	r.Register(plugin.Plugin[testInterface]{Name: "b", Instance: externalImpl{"b"}, Priority: 2})
	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
}

func TestRegistryIsDefault(t *testing.T) {
	r := plugin.New[testInterface]("test", builtinImpl{})
	if !r.IsDefault() {
		t.Fatal("expected IsDefault=true with no plugins")
	}
	r.Register(plugin.Plugin[testInterface]{Name: "x", Instance: externalImpl{"x"}, Priority: 1})
	if r.IsDefault() {
		t.Fatal("expected IsDefault=false after registering plugin")
	}
}

func TestRegistryRegisterEmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	r := plugin.New[testInterface]("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "", Instance: externalImpl{"x"}, Priority: 1})
}
