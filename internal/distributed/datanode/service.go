// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcdatanode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	ot "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	dn "github.com/milvus-io/milvus/internal/datanode"
	dcc "github.com/milvus-io/milvus/internal/distributed/datacoord/client"
	rcc "github.com/milvus-io/milvus/internal/distributed/rootcoord/client"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/funcutil"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/trace"
	"github.com/milvus-io/milvus/internal/util/typeutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

var Params paramtable.GrpcServerConfig

type Server struct {
	datanode    types.DataNodeComponent
	wg          sync.WaitGroup
	grpcErrChan chan error
	grpcServer  *grpc.Server
	ctx         context.Context
	cancel      context.CancelFunc

	msFactory msgstream.Factory

	rootCoord types.RootCoord
	dataCoord types.DataCoord

	newRootCoordClient func(string, []string) (types.RootCoord, error)
	newDataCoordClient func(string, []string) (types.DataCoord, error)

	closer io.Closer
}

// NewServer new data node grpc server
func NewServer(ctx context.Context, factory msgstream.Factory) (*Server, error) {
	ctx1, cancel := context.WithCancel(ctx)
	var s = &Server{
		ctx:         ctx1,
		cancel:      cancel,
		msFactory:   factory,
		grpcErrChan: make(chan error),
		newRootCoordClient: func(etcdMetaRoot string, etcdEndpoints []string) (types.RootCoord, error) {
			return rcc.NewClient(ctx1, etcdMetaRoot, etcdEndpoints)
		},
		newDataCoordClient: func(etcdMetaRoot string, etcdEndpoints []string) (types.DataCoord, error) {
			return dcc.NewClient(ctx1, etcdMetaRoot, etcdEndpoints)
		},
	}

	s.datanode = dn.NewDataNode(s.ctx, s.msFactory)

	return s, nil
}

func (s *Server) startGrpc() error {
	s.wg.Add(1)
	go s.startGrpcLoop(Params.Listener)
	// wait for grpc server loop start
	err := <-s.grpcErrChan
	return err
}

// startGrpcLoop starts the grep loop of datanode component.
func (s *Server) startGrpcLoop(listener net.Listener) {
	defer s.wg.Done()
	var kaep = keepalive.EnforcementPolicy{
		MinTime:             5 * time.Second, // If a client pings more than once every 5 seconds, terminate the connection
		PermitWithoutStream: true,            // Allow pings even when there are no active streams
	}

	var kasp = keepalive.ServerParameters{
		Time:    60 * time.Second, // Ping the client if it is idle for 60 seconds to ensure the connection is still active
		Timeout: 10 * time.Second, // Wait 10 second for the ping ack before assuming the connection is dead
	}

	opts := trace.GetInterceptorOpts()
	s.grpcServer = grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(kaep),
		grpc.KeepaliveParams(kasp),
		grpc.MaxRecvMsgSize(Params.ServerMaxRecvSize),
		grpc.MaxSendMsgSize(Params.ServerMaxSendSize),
		grpc.UnaryInterceptor(ot.UnaryServerInterceptor(opts...)),
		grpc.StreamInterceptor(ot.StreamServerInterceptor(opts...)))
	datapb.RegisterDataNodeServer(s.grpcServer, s)

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	go funcutil.CheckGrpcReady(ctx, s.grpcErrChan)
	if err := s.grpcServer.Serve(listener); err != nil {
		log.Warn("DataNode Start Grpc Failed!")
		s.grpcErrChan <- err
	}

}

func (s *Server) SetRootCoordInterface(ms types.RootCoord) error {
	return s.datanode.SetRootCoord(ms)
}

func (s *Server) SetDataCoordInterface(ds types.DataCoord) error {
	return s.datanode.SetDataCoord(ds)
}

// Run initializes and starts Datanode's grpc service.
func (s *Server) Run() error {
	if err := s.init(); err != nil {
		return err
	}
	log.Debug("DataNode init done ...")

	if err := s.start(); err != nil {
		return err
	}
	log.Debug("DataNode start done ...")
	return nil
}

// Stop stops Datanode's grpc service.
func (s *Server) Stop() error {
	log.Debug("Datanode stop", zap.String("Address", Params.GetAddress()))
	if s.closer != nil {
		if err := s.closer.Close(); err != nil {
			return err
		}
	}
	s.cancel()
	if s.grpcServer != nil {
		log.Debug("Graceful stop grpc server...")
		// make graceful stop has a timeout
		stopped := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(stopped)
		}()

		t := time.NewTimer(10 * time.Second)
		select {
		case <-t.C:
			// hard stop since grace timeout
			s.grpcServer.Stop()
		case <-stopped:
			t.Stop()
		}
	}

	err := s.datanode.Stop()
	if err != nil {
		return err
	}
	s.wg.Wait()
	return nil
}

// init initializes Datanode's grpc service.
func (s *Server) init() error {
	ctx := context.Background()
	Params.InitOnce(typeutil.DataNodeRole)

	dn.Params.InitOnce()
	dn.Params.DataNodeCfg.Port = Params.Port
	dn.Params.DataNodeCfg.IP = Params.IP

	closer := trace.InitTracing(fmt.Sprintf("data_node ip: %s, port: %d", Params.IP, Params.Port))
	s.closer = closer
	addr := Params.IP + ":" + strconv.Itoa(Params.Port)
	log.Debug("DataNode address", zap.String("address", addr))

	err := s.startGrpc()
	if err != nil {
		return err
	}

	// --- RootCoord Client ---
	if s.newRootCoordClient != nil {
		log.Debug("Init root coord client ...")
		rootCoordClient, err := s.newRootCoordClient(dn.Params.DataNodeCfg.MetaRootPath, dn.Params.DataNodeCfg.EtcdEndpoints)
		if err != nil {
			log.Debug("DataNode newRootCoordClient failed", zap.Error(err))
			panic(err)
		}
		if err = rootCoordClient.Init(); err != nil {
			log.Debug("DataNode rootCoordClient Init failed", zap.Error(err))
			panic(err)
		}
		if err = rootCoordClient.Start(); err != nil {
			log.Debug("DataNode rootCoordClient Start failed", zap.Error(err))
			panic(err)
		}
		err = funcutil.WaitForComponentHealthy(ctx, rootCoordClient, "RootCoord", 1000000, time.Millisecond*200)
		if err != nil {
			log.Debug("DataNode wait rootCoord ready failed", zap.Error(err))
			panic(err)
		}
		log.Debug("DataNode rootCoord is ready")
		if err = s.SetRootCoordInterface(rootCoordClient); err != nil {
			panic(err)
		}
	}

	// --- Data Server Client ---
	if s.newDataCoordClient != nil {
		log.Debug("DataNode Init data service client ...")
		dataCoordClient, err := s.newDataCoordClient(dn.Params.DataNodeCfg.MetaRootPath, dn.Params.DataNodeCfg.EtcdEndpoints)
		if err != nil {
			log.Debug("DataNode newDataCoordClient failed", zap.Error(err))
			panic(err)
		}
		if err = dataCoordClient.Init(); err != nil {
			log.Debug("DataNode newDataCoord failed", zap.Error(err))
			panic(err)
		}
		if err = dataCoordClient.Start(); err != nil {
			log.Debug("DataNode dataCoordClient Start failed", zap.Error(err))
			panic(err)
		}
		err = funcutil.WaitForComponentInitOrHealthy(ctx, dataCoordClient, "DataCoord", 1000000, time.Millisecond*200)
		if err != nil {
			log.Debug("DataNode wait dataCoordClient ready failed", zap.Error(err))
			panic(err)
		}
		log.Debug("DataNode dataCoord is ready")
		if err = s.SetDataCoordInterface(dataCoordClient); err != nil {
			panic(err)
		}
	}

	s.datanode.SetNodeID(dn.Params.DataNodeCfg.NodeID)
	s.datanode.UpdateStateCode(internalpb.StateCode_Initializing)

	if err := s.datanode.Init(); err != nil {
		log.Warn("datanode init error: ", zap.Error(err))
		return err
	}
	log.Debug("DataNode", zap.Any("State", internalpb.StateCode_Initializing))
	return nil
}

// start starts datanode's grpc service.
func (s *Server) start() error {
	if err := s.datanode.Start(); err != nil {
		return err
	}
	err := s.datanode.Register()
	if err != nil {
		log.Debug("DataNode Register etcd failed", zap.Error(err))
		return err
	}
	return nil
}

// GetComponentStates gets the component states of Datanode
func (s *Server) GetComponentStates(ctx context.Context, req *internalpb.GetComponentStatesRequest) (*internalpb.ComponentStates, error) {
	return s.datanode.GetComponentStates(ctx)
}

// GetStatisticsChannel gets the statistics channel of Datanode.
func (s *Server) GetStatisticsChannel(ctx context.Context, req *internalpb.GetStatisticsChannelRequest) (*milvuspb.StringResponse, error) {
	return s.datanode.GetStatisticsChannel(ctx)
}

// Deprecated
func (s *Server) WatchDmChannels(ctx context.Context, req *datapb.WatchDmChannelsRequest) (*commonpb.Status, error) {
	return s.datanode.WatchDmChannels(ctx, req)
}

func (s *Server) FlushSegments(ctx context.Context, req *datapb.FlushSegmentsRequest) (*commonpb.Status, error) {
	if s.datanode.GetStateCode() != internalpb.StateCode_Healthy {
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    "DataNode isn't healthy.",
		}, errors.New("DataNode is not ready yet")
	}
	return s.datanode.FlushSegments(ctx, req)
}

// GetMetrics gets the metrics info of Datanode.
func (s *Server) GetMetrics(ctx context.Context, request *milvuspb.GetMetricsRequest) (*milvuspb.GetMetricsResponse, error) {
	return s.datanode.GetMetrics(ctx, request)
}

func (s *Server) Compaction(ctx context.Context, request *datapb.CompactionPlan) (*commonpb.Status, error) {
	return s.datanode.Compaction(ctx, request)
}
