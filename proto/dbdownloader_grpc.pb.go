// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.3.0
// - protoc             (unknown)
// source: proto/dbdownloader.proto

package proto

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

const (
	Downloader_DownloadFile_FullMethodName   = "/proto.Downloader/DownloadFile"
	Downloader_DownloadChunks_FullMethodName = "/proto.Downloader/DownloadChunks"
)

// DownloaderClient is the client API for Downloader service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type DownloaderClient interface {
	DownloadFile(ctx context.Context, in *DownloadFileRequest, opts ...grpc.CallOption) (Downloader_DownloadFileClient, error)
	DownloadChunks(ctx context.Context, in *DownloadChunksRequest, opts ...grpc.CallOption) (Downloader_DownloadChunksClient, error)
}

type downloaderClient struct {
	cc grpc.ClientConnInterface
}

func NewDownloaderClient(cc grpc.ClientConnInterface) DownloaderClient {
	return &downloaderClient{cc}
}

func (c *downloaderClient) DownloadFile(ctx context.Context, in *DownloadFileRequest, opts ...grpc.CallOption) (Downloader_DownloadFileClient, error) {
	stream, err := c.cc.NewStream(ctx, &Downloader_ServiceDesc.Streams[0], Downloader_DownloadFile_FullMethodName, opts...)
	if err != nil {
		return nil, err
	}
	x := &downloaderDownloadFileClient{stream}
	if err := x.ClientStream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}

type Downloader_DownloadFileClient interface {
	Recv() (*DownloadFileResponse, error)
	grpc.ClientStream
}

type downloaderDownloadFileClient struct {
	grpc.ClientStream
}

func (x *downloaderDownloadFileClient) Recv() (*DownloadFileResponse, error) {
	m := new(DownloadFileResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *downloaderClient) DownloadChunks(ctx context.Context, in *DownloadChunksRequest, opts ...grpc.CallOption) (Downloader_DownloadChunksClient, error) {
	stream, err := c.cc.NewStream(ctx, &Downloader_ServiceDesc.Streams[1], Downloader_DownloadChunks_FullMethodName, opts...)
	if err != nil {
		return nil, err
	}
	x := &downloaderDownloadChunksClient{stream}
	if err := x.ClientStream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}

type Downloader_DownloadChunksClient interface {
	Recv() (*DownloadChunksResponse, error)
	grpc.ClientStream
}

type downloaderDownloadChunksClient struct {
	grpc.ClientStream
}

func (x *downloaderDownloadChunksClient) Recv() (*DownloadChunksResponse, error) {
	m := new(DownloadChunksResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// DownloaderServer is the server API for Downloader service.
// All implementations should embed UnimplementedDownloaderServer
// for forward compatibility
type DownloaderServer interface {
	DownloadFile(*DownloadFileRequest, Downloader_DownloadFileServer) error
	DownloadChunks(*DownloadChunksRequest, Downloader_DownloadChunksServer) error
}

// UnimplementedDownloaderServer should be embedded to have forward compatible implementations.
type UnimplementedDownloaderServer struct {
}

func (UnimplementedDownloaderServer) DownloadFile(*DownloadFileRequest, Downloader_DownloadFileServer) error {
	return status.Errorf(codes.Unimplemented, "method DownloadFile not implemented")
}
func (UnimplementedDownloaderServer) DownloadChunks(*DownloadChunksRequest, Downloader_DownloadChunksServer) error {
	return status.Errorf(codes.Unimplemented, "method DownloadChunks not implemented")
}

// UnsafeDownloaderServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to DownloaderServer will
// result in compilation errors.
type UnsafeDownloaderServer interface {
	mustEmbedUnimplementedDownloaderServer()
}

func RegisterDownloaderServer(s grpc.ServiceRegistrar, srv DownloaderServer) {
	s.RegisterService(&Downloader_ServiceDesc, srv)
}

func _Downloader_DownloadFile_Handler(srv interface{}, stream grpc.ServerStream) error {
	m := new(DownloadFileRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(DownloaderServer).DownloadFile(m, &downloaderDownloadFileServer{stream})
}

type Downloader_DownloadFileServer interface {
	Send(*DownloadFileResponse) error
	grpc.ServerStream
}

type downloaderDownloadFileServer struct {
	grpc.ServerStream
}

func (x *downloaderDownloadFileServer) Send(m *DownloadFileResponse) error {
	return x.ServerStream.SendMsg(m)
}

func _Downloader_DownloadChunks_Handler(srv interface{}, stream grpc.ServerStream) error {
	m := new(DownloadChunksRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(DownloaderServer).DownloadChunks(m, &downloaderDownloadChunksServer{stream})
}

type Downloader_DownloadChunksServer interface {
	Send(*DownloadChunksResponse) error
	grpc.ServerStream
}

type downloaderDownloadChunksServer struct {
	grpc.ServerStream
}

func (x *downloaderDownloadChunksServer) Send(m *DownloadChunksResponse) error {
	return x.ServerStream.SendMsg(m)
}

// Downloader_ServiceDesc is the grpc.ServiceDesc for Downloader service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var Downloader_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "proto.Downloader",
	HandlerType: (*DownloaderServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "DownloadFile",
			Handler:       _Downloader_DownloadFile_Handler,
			ServerStreams: true,
		},
		{
			StreamName:    "DownloadChunks",
			Handler:       _Downloader_DownloadChunks_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "proto/dbdownloader.proto",
}
