// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package grpc

// xfer_service.go 是 gRPC 传输的 Xfer 服务装配（roadmap P2 gRPC 传输装配）：
//
//   - 手写 ServiceDesc（无 protoc 生成）：Xfer.Stream 双向流。
//   - 消息 XferMsg（骨架已定义，bytes payload 直传）。
//   - Dial 用 grpc.NewClient + NewStream；Listen 用 grpc.NewServer + RegisterService。
//
// 用 grpc-go（google.golang.org/grpc v1.84）。

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// xferServiceName 是 Xfer 服务全名。
const xferServiceName = "sproxy.xfer.Xfer"

// xferServer 实现 Xfer.Stream 服务端（骨架 Xfer_StreamServer 接口）。
type xferServer struct {
	handler func(stream Xfer_StreamServer) error
}

func (x *xferServer) Stream(srv Xfer_StreamServer) error {
	return x.handler(srv)
}

// anyXferServer 是 HandlerType 标记接口（grpc 反射校验用）。
type anyXferServer any

// xferServiceDesc 是手写 ServiceDesc。
var xferServiceDesc = grpc.ServiceDesc{
	ServiceName: xferServiceName,
	HandlerType: (*anyXferServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "Stream",
			Handler:       xferStreamHandler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "xfer.proto",
}

// xferStreamHandler 是 gRPC 流 handler（适配骨架接口）。
func xferStreamHandler(srv any, stream grpc.ServerStream) error {
	s, ok := srv.(*xferServer)
	if !ok {
		return status.Error(codes.Internal, "xfer: 类型断言失败")
	}
	return s.Stream(&serverStreamAdapter{stream})
}

// serverStreamAdapter 适配 grpc.ServerStream 为骨架 Xfer_StreamServer。
type serverStreamAdapter struct {
	s grpc.ServerStream
}

func (a *serverStreamAdapter) Send(m *XferMsg) error { return a.s.SendMsg(m) }
func (a *serverStreamAdapter) Recv() (*XferMsg, error) {
	m := &XferMsg{}
	if err := a.s.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}
func (a *serverStreamAdapter) Context() context.Context { return a.s.Context() }

// clientStreamAdapter 适配 grpc.ClientStream 为骨架 Xfer_StreamClient。
type clientStreamAdapter struct {
	s grpc.ClientStream
}

func (a *clientStreamAdapter) Send(m *XferMsg) error { return a.s.SendMsg(m) }
func (a *clientStreamAdapter) Recv() (*XferMsg, error) {
	m := &XferMsg{}
	if err := a.s.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}
func (a *clientStreamAdapter) CloseSend() error         { return a.s.CloseSend() }
func (a *clientStreamAdapter) Context() context.Context { return a.s.Context() }

// grpcListener 实现 xfer.Listener（grpc.Server 包装）。
type grpcListener struct {
	srv *grpc.Server
	ln  net.Listener
	ch  chan Xfer_StreamServer
}

func (l *grpcListener) Accept(ctx context.Context) (Conn, error) {
	select {
	case stream := <-l.ch:
		return &grpcConn{stream: stream}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *grpcListener) Close() error {
	l.srv.Stop()
	return l.ln.Close()
}

func (l *grpcListener) Addr() string { return l.ln.Addr().String() }

// grpcConn 实现 xfer.Conn（骨架 grpcConn 已有——复用）。
