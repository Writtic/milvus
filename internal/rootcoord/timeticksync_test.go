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

package rootcoord

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
)

func TestTimetickSync(t *testing.T) {
	ctx := context.Background()

	session := &sessionutil.Session{
		ServerID: 100,
	}

	factory := msgstream.NewPmsFactory()
	m := map[string]interface{}{
		"pulsarAddress":  Params.RootCoordCfg.PulsarAddress,
		"receiveBufSize": 1024,
		"pulsarBufSize":  1024}
	err := factory.SetParams(m)
	assert.Nil(t, err)

	//chanMap := map[typeutil.UniqueID][]string{
	//	int64(1): {"rootcoord-dml_0"},
	//}

	Params.RootCoordCfg.DmlChannelNum = 2
	Params.RootCoordCfg.DmlChannelName = "rootcoord-dml"
	Params.RootCoordCfg.DeltaChannelName = "rootcoord-delta"
	ttSync := newTimeTickSync(ctx, session, factory, nil)

	t.Run("sendToChannel", func(t *testing.T) {
		ttSync.sendToChannel()

		ttSync.proxyTimeTick[1] = nil
		ttSync.sendToChannel()

		msg := &internalpb.ChannelTimeTickMsg{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_TimeTick,
			},
		}
		ttSync.proxyTimeTick[1] = newChanTsMsg(msg, 1)
		ttSync.sendToChannel()
	})

	t.Run("RemoveDdlTimeTick", func(t *testing.T) {
		ttSync.addDdlTimeTick(uint64(1), "1")
		ttSync.addDdlTimeTick(uint64(2), "2")
		ttSync.removeDdlTimeTick(uint64(1), "1")
		assert.Equal(t, ttSync.ddlMinTs, uint64(2))
	})

	t.Run("UpdateTimeTick", func(t *testing.T) {
		msg := &internalpb.ChannelTimeTickMsg{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_TimeTick,
				SourceID: int64(1),
			},
			DefaultTimestamp: 0,
		}

		err := ttSync.updateTimeTick(msg, "1")
		assert.Nil(t, err)

		msg.ChannelNames = append(msg.ChannelNames, "a")
		err = ttSync.updateTimeTick(msg, "1")
		assert.Error(t, err)

		msg.Timestamps = append(msg.Timestamps, uint64(2))
		msg.DefaultTimestamp = uint64(200)
		cttMsg := newChanTsMsg(msg, 1)
		ttSync.proxyTimeTick[msg.Base.SourceID] = cttMsg

		ttSync.ddlMinTs = uint64(100)
		err = ttSync.updateTimeTick(msg, "1")
		assert.Nil(t, err)

		ttSync.ddlMinTs = uint64(300)
		ttSync.session.ServerID = int64(1)
		err = ttSync.updateTimeTick(msg, "1")
		assert.Nil(t, err)
	})

	t.Run("minTimeTick", func(t *testing.T) {
		tts := make([]uint64, 2)
		tts[0] = uint64(5)
		tts[1] = uint64(3)

		ret := minTimeTick(tts...)
		assert.Equal(t, ret, tts[1])
	})
}
