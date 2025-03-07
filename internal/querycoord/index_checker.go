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

package querycoord

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/golang/protobuf/proto"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/kv"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/indexpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/types"
)

type indexInfo struct {
	segmentID    UniqueID
	collectionID UniqueID
	partitionID  UniqueID

	infos       []*indexpb.IndexFilePathInfo
	enableIndex bool
}

// IndexChecker checks index
type IndexChecker struct {
	ctx    context.Context
	cancel context.CancelFunc
	client kv.MetaKv

	revision int64

	handoffReqChan        chan *querypb.SegmentInfo
	unIndexedSegmentsChan chan *querypb.SegmentInfo
	indexedSegmentsChan   chan *querypb.SegmentInfo

	meta      Meta
	scheduler *TaskScheduler
	cluster   Cluster

	rootCoord  types.RootCoord
	indexCoord types.IndexCoord
	dataCoord  types.DataCoord

	wg sync.WaitGroup
}

func newIndexChecker(ctx context.Context,
	client kv.MetaKv, meta Meta, cluster Cluster, scheduler *TaskScheduler,
	root types.RootCoord, index types.IndexCoord, data types.DataCoord) (*IndexChecker, error) {
	childCtx, cancel := context.WithCancel(ctx)
	reqChan := make(chan *querypb.SegmentInfo, 1024)
	unIndexChan := make(chan *querypb.SegmentInfo, 1024)
	indexedChan := make(chan *querypb.SegmentInfo, 1024)

	checker := &IndexChecker{
		ctx:    childCtx,
		cancel: cancel,
		client: client,

		handoffReqChan:        reqChan,
		unIndexedSegmentsChan: unIndexChan,
		indexedSegmentsChan:   indexedChan,

		meta:      meta,
		scheduler: scheduler,
		cluster:   cluster,

		rootCoord:  root,
		indexCoord: index,
		dataCoord:  data,
	}
	err := checker.reloadFromKV()
	if err != nil {
		log.Error("index checker reload from kv failed", zap.Error(err))
		return nil, err
	}

	return checker, nil
}

func (ic *IndexChecker) start() {
	ic.wg.Add(2)
	go ic.checkIndexLoop()
	go ic.processHandoffAfterIndexDone()
}

func (ic *IndexChecker) close() {
	ic.cancel()
	ic.wg.Wait()
}

// reloadFromKV  reload collection/partition, remove from etcd if failed.
func (ic *IndexChecker) reloadFromKV() error {
	_, handoffReqValues, version, err := ic.client.LoadWithRevision(handoffSegmentPrefix)
	if err != nil {
		log.Error("reloadFromKV: LoadWithRevision from kv failed", zap.Error(err))
		return err
	}
	ic.revision = version

	for _, value := range handoffReqValues {
		segmentInfo := &querypb.SegmentInfo{}
		err := proto.Unmarshal([]byte(value), segmentInfo)
		if err != nil {
			log.Error("reloadFromKV: unmarshal failed", zap.Any("error", err.Error()))
			return err
		}
		if ic.verifyHandoffReqValid(segmentInfo) && Params.QueryCoordCfg.AutoHandoff {
			// push the req to handoffReqChan and then wait to load after index created
			// in case handoffReqChan is full, and block start process
			go ic.enqueueHandoffReq(segmentInfo)
		} else {
			log.Debug("reloadFromKV: collection/partition has not been loaded, remove req from etcd", zap.Any("segmentInfo", segmentInfo))
			buildQuerySegmentPath := fmt.Sprintf("%s/%d/%d/%d", handoffSegmentPrefix, segmentInfo.CollectionID, segmentInfo.PartitionID, segmentInfo.SegmentID)
			err = ic.client.Remove(buildQuerySegmentPath)
			if err != nil {
				log.Error("reloadFromKV: remove handoff segment from etcd failed", zap.Error(err))
				return err
			}
		}
		log.Debug("reloadFromKV: process handoff request done", zap.Any("segmentInfo", segmentInfo))
	}

	return nil
}

func (ic *IndexChecker) verifyHandoffReqValid(req *querypb.SegmentInfo) bool {
	// if collection has not been loaded, then skip the segment
	collectionInfo, err := ic.meta.getCollectionInfoByID(req.CollectionID)
	if err == nil {
		// if partition has not been loaded or released, then skip handoff the segment
		if collectionInfo.LoadType == querypb.LoadType_LoadPartition {
			for _, id := range collectionInfo.PartitionIDs {
				if id == req.PartitionID {
					return true
				}
			}
		} else {
			partitionReleased := false
			for _, id := range collectionInfo.ReleasedPartitionIDs {
				if id == req.PartitionID {
					partitionReleased = true
				}
			}
			if !partitionReleased {
				return true
			}
		}
	}

	return false
}

func (ic *IndexChecker) enqueueHandoffReq(req *querypb.SegmentInfo) {
	ic.handoffReqChan <- req
}

func (ic *IndexChecker) enqueueUnIndexSegment(info *querypb.SegmentInfo) {
	ic.unIndexedSegmentsChan <- info
}

func (ic *IndexChecker) enqueueIndexedSegment(info *querypb.SegmentInfo) {
	ic.indexedSegmentsChan <- info
}

func (ic *IndexChecker) checkIndexLoop() {
	defer ic.wg.Done()

	for {
		select {
		case <-ic.ctx.Done():
			return
		case segmentInfo := <-ic.handoffReqChan:
			// TODO:: check whether the index exists in parallel, in case indexCoord cannot create the index normally, and then block the loop
			log.Debug("checkIndexLoop: start check index for handoff segment", zap.Int64("segmentID", segmentInfo.SegmentID))
			for {
				if ic.verifyHandoffReqValid(segmentInfo) && Params.QueryCoordCfg.AutoHandoff {
					indexInfo, err := getIndexInfo(ic.ctx, segmentInfo, ic.rootCoord, ic.indexCoord)
					if err != nil {
						continue
					}
					if indexInfo.enableIndex {
						segmentInfo.EnableIndex = true
					}
					segmentInfo.IndexPathInfos = indexInfo.infos
					ic.enqueueIndexedSegment(segmentInfo)
					break
				}

				buildQuerySegmentPath := fmt.Sprintf("%s/%d/%d/%d", handoffSegmentPrefix, segmentInfo.CollectionID, segmentInfo.PartitionID, segmentInfo.SegmentID)
				err := ic.client.Remove(buildQuerySegmentPath)
				if err != nil {
					log.Error("checkIndexLoop: remove handoff segment from etcd failed", zap.Error(err))
					panic(err)
				}
				break
			}
		case segmentInfo := <-ic.unIndexedSegmentsChan:
			//TODO:: check index after load collection/partition, some segments may don't has index when loading
			log.Debug("checkIndexLoop: start check index for segment which has not loaded index", zap.Int64("segmentID", segmentInfo.SegmentID))

		}
	}
}

func (ic *IndexChecker) processHandoffAfterIndexDone() {
	defer ic.wg.Done()

	for {
		select {
		case <-ic.ctx.Done():
			return
		case segmentInfo := <-ic.indexedSegmentsChan:
			collectionID := segmentInfo.CollectionID
			partitionID := segmentInfo.PartitionID
			segmentID := segmentInfo.SegmentID
			log.Debug("processHandoffAfterIndexDone: handoff segment start", zap.Any("segmentInfo", segmentInfo))
			baseTask := newBaseTask(ic.ctx, querypb.TriggerCondition_Handoff)
			handoffReq := &querypb.HandoffSegmentsRequest{
				Base: &commonpb.MsgBase{
					MsgType: commonpb.MsgType_HandoffSegments,
				},
				SegmentInfos: []*querypb.SegmentInfo{segmentInfo},
			}
			handoffTask := &handoffTask{
				baseTask:               baseTask,
				HandoffSegmentsRequest: handoffReq,
				dataCoord:              ic.dataCoord,
				cluster:                ic.cluster,
				meta:                   ic.meta,
			}
			err := ic.scheduler.Enqueue(handoffTask)
			if err != nil {
				log.Error("processHandoffAfterIndexDone: handoffTask enqueue failed", zap.Error(err))
				panic(err)
			}

			go func() {
				err := handoffTask.waitToFinish()
				if err != nil {
					// collection or partition may have been released before handoffTask enqueue
					log.Warn("processHandoffAfterIndexDone: handoffTask failed", zap.Error(err))
				}

				log.Debug("processHandoffAfterIndexDone: handoffTask completed", zap.Any("segment infos", handoffTask.SegmentInfos))
			}()

			// once task enqueue, etcd data can be cleaned, handoffTask will recover from taskScheduler's reloadFromKV()
			buildQuerySegmentPath := fmt.Sprintf("%s/%d/%d/%d", handoffSegmentPrefix, collectionID, partitionID, segmentID)
			err = ic.client.Remove(buildQuerySegmentPath)
			if err != nil {
				log.Error("processHandoffAfterIndexDone: remove handoff segment from etcd failed", zap.Error(err))
				panic(err)
			}
		}
	}
}

func getIndexInfo(ctx context.Context, info *querypb.SegmentInfo, root types.RootCoord, index types.IndexCoord) (*indexInfo, error) {
	indexInfo := &indexInfo{
		segmentID:    info.SegmentID,
		partitionID:  info.PartitionID,
		collectionID: info.CollectionID,
	}

	// check the buildID of the segment's index whether exist on rootCoord
	req := &milvuspb.DescribeSegmentRequest{
		Base: &commonpb.MsgBase{
			MsgType: commonpb.MsgType_DescribeSegment,
		},
		CollectionID: info.CollectionID,
		SegmentID:    info.SegmentID,
	}
	ctx2, cancel2 := context.WithTimeout(ctx, timeoutForRPC)
	defer cancel2()
	response, err := root.DescribeSegment(ctx2, req)
	if err != nil {
		return nil, err
	}
	if response.Status.ErrorCode != commonpb.ErrorCode_Success {
		return nil, errors.New(response.Status.Reason)
	}

	// if the segment.EnableIndex == false, then load the segment immediately
	if !response.EnableIndex {
		indexInfo.enableIndex = false
		return indexInfo, nil
	}

	// if index created done on indexNode, then handoff start
	indexFilePathRequest := &indexpb.GetIndexFilePathsRequest{
		IndexBuildIDs: []UniqueID{response.BuildID},
	}
	ctx3, cancel3 := context.WithTimeout(ctx, timeoutForRPC)
	defer cancel3()
	pathResponse, err2 := index.GetIndexFilePaths(ctx3, indexFilePathRequest)
	if err2 != nil {
		return nil, err2
	}

	if pathResponse.Status.ErrorCode != commonpb.ErrorCode_Success {
		return nil, errors.New(pathResponse.Status.Reason)
	}

	if len(pathResponse.FilePaths) <= 0 {
		return nil, errors.New("illegal index file paths")
	}
	for _, fieldPath := range pathResponse.FilePaths {
		if len(fieldPath.IndexFilePaths) == 0 {
			return nil, errors.New("empty index paths")
		}
	}

	indexInfo.enableIndex = true
	indexInfo.infos = pathResponse.FilePaths

	return indexInfo, nil
}
