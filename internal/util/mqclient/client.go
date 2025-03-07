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

// Client is the interface that provides operations of message queues
type Client interface {
	// CreateReader creates a reader instance
	CreateReader(options ReaderOptions) (Reader, error)

	// CreateProducer creates a producer instance
	CreateProducer(options ProducerOptions) (Producer, error)

	// Subscribe creates a consumer instance and subscribe a topic
	Subscribe(options ConsumerOptions) (Consumer, error)

	// Get the earliest MessageID
	EarliestMessageID() MessageID

	// String to msg ID
	StringToMsgID(string) (MessageID, error)

	// Deserialize MessageId from a byte array
	BytesToMsgID([]byte) (MessageID, error)

	// Close the client and free associated resources
	Close()
}
