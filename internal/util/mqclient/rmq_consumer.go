// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package mqclient

import (
	"sync"

	"github.com/milvus-io/milvus/internal/util/rocksmq/client/rocksmq"
)

// RmqConsumer is a client that used to consume messages from rocksmq
type RmqConsumer struct {
	c          rocksmq.Consumer
	msgChannel chan Message
	closeCh    chan struct{}
	once       sync.Once
	skip       bool
}

// Subscription returns the subscription name of this consumer
func (rc *RmqConsumer) Subscription() string {
	return rc.c.Subscription()
}

// Chan returns a channel to read messages from rocksmq
func (rc *RmqConsumer) Chan() <-chan Message {
	if rc.msgChannel == nil {
		rc.once.Do(func() {
			rc.msgChannel = make(chan Message, 256)
			go func() {
				for { //nolint:gosimple
					select {
					case msg, ok := <-rc.c.Chan():
						if !ok {
							close(rc.msgChannel)
							return
						}
						if !rc.skip {
							rc.msgChannel <- &rmqMessage{msg: msg}
						} else {
							rc.skip = false
						}
					case <-rc.closeCh:
						close(rc.msgChannel)
						return
					}
				}
			}()
		})
	}
	return rc.msgChannel
}

// Seek is used to seek the position in rocksmq topic
func (rc *RmqConsumer) Seek(id MessageID, inclusive bool) error {
	msgID := id.(*rmqID).messageID
	// skip the first message when consume
	rc.skip = !inclusive
	return rc.c.Seek(msgID)
}

// Ack is used to ask a rocksmq message
func (rc *RmqConsumer) Ack(message Message) {
}

// Close is used to free the resources of this consumer
func (rc *RmqConsumer) Close() {
	rc.c.Close()
	close(rc.closeCh)
}
