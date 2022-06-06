// Copyright © 2022 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jetstream

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	sdk "github.com/conduitio/conduit-connector-sdk"
	"github.com/nats-io/nats.go"
)

// heartbeatTimeout is a default heartbeat timeout for push consumers.
const heartbeatTimeout = 2 * time.Second

// Iterator is a iterator for JetStream communication model.
// It receives message from NATS JetStream.
type Iterator struct {
	sync.Mutex

	conn          *nats.Conn
	messages      chan *nats.Msg
	unackMessages []*nats.Msg
	jetstream     nats.JetStreamContext
	consumerInfo  *nats.ConsumerInfo
	subscription  *nats.Subscription
}

// IteratorParams contains incoming params for the NewIterator function.
type IteratorParams struct {
	Conn          *nats.Conn
	BufferSize    int
	Durable       string
	Stream        string
	Subject       string
	SDKPosition   sdk.Position
	DeliverPolicy nats.DeliverPolicy
	AckPolicy     nats.AckPolicy
}

// NewIterator creates new instance of the Iterator.
func NewIterator(ctx context.Context, params IteratorParams) (*Iterator, error) {
	jetstream, err := params.Conn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("get jetstream context: %w", err)
	}

	consumerConfig, err := getConsumerConfig(params)
	if err != nil {
		return nil, fmt.Errorf("get consumer config: %w", err)
	}

	consumerInfo, err := jetstream.AddConsumer(params.Stream, consumerConfig)
	if err != nil {
		return nil, fmt.Errorf("add jetstream consumer: %w", err)
	}

	messages := make(chan *nats.Msg, params.BufferSize)

	subscription, err := jetstream.ChanSubscribe(params.Subject, messages,
		nats.Durable(consumerInfo.Config.Durable),
	)
	if err != nil {
		return nil, fmt.Errorf("chan subscribe: %w", err)
	}

	return &Iterator{
		conn:          params.Conn,
		messages:      messages,
		unackMessages: make([]*nats.Msg, 0),
		jetstream:     jetstream,
		consumerInfo:  consumerInfo,
		subscription:  subscription,
	}, nil
}

// HasNext checks is the iterator has messages.
func (i *Iterator) HasNext(ctx context.Context) bool {
	return len(i.messages) > 0
}

// Next returns the next record from the underlying messages channel.
// It also appends messages to a unackMessages slice if the AckPolicy is not equal to AckNonePolicy.
func (i *Iterator) Next(ctx context.Context) (sdk.Record, error) {
	select {
	case msg := <-i.messages:
		sdkRecord, err := i.messageToRecord(msg)
		if err != nil {
			return sdk.Record{}, fmt.Errorf("convert message to record: %w", err)
		}

		if i.consumerInfo.Config.AckPolicy != nats.AckNonePolicy {
			i.Lock()
			i.unackMessages = append(i.unackMessages, msg)
			i.Unlock()
		}

		return sdkRecord, nil

	case <-ctx.Done():
		return sdk.Record{}, ctx.Err()
	}
}

// Ack acknowledges a message at the given position.
func (i *Iterator) Ack(ctx context.Context, sdkPosition sdk.Position) error {
	// if ack policy is 'none' just return nil here
	if i.consumerInfo.Config.AckPolicy == nats.AckNonePolicy {
		return nil
	}

	i.Lock()
	defer i.Unlock()

	if err := i.canAck(sdkPosition); err != nil {
		return fmt.Errorf("message cannot be acknowledged: %w", err)
	}

	if err := i.unackMessages[0].Ack(); err != nil {
		return fmt.Errorf("ack message: %w", err)
	}

	// remove acknowledged message from the slice
	i.unackMessages = i.unackMessages[1:]

	return nil
}

// Stop stops the Iterator, unsubscribes from a subject.
func (i *Iterator) Stop() (err error) {
	if i.subscription != nil {
		if err = i.subscription.Unsubscribe(); err != nil {
			return fmt.Errorf("unsubscribe: %w", err)
		}
	}

	close(i.messages)

	if i.jetstream != nil {
		err := i.jetstream.DeleteConsumer(
			i.consumerInfo.Stream,
			i.consumerInfo.Name,
		)
		if err != nil {
			return fmt.Errorf("delete consumer: %w", err)
		}
	}

	if i.conn != nil {
		i.conn.Close()
	}

	return nil
}

// getConsumerConfig returns a *nats.ConsumerConfig based on the incoming params and a sdk.Position.
func getConsumerConfig(params IteratorParams) (*nats.ConsumerConfig, error) {
	position, err := parsePosition(params.SDKPosition)
	if err != nil {
		return nil, fmt.Errorf("parse position: %w", err)
	}

	var (
		deliverPolicy = params.DeliverPolicy
		startSeq      uint64
	)

	// if the position has a non-zero OptSeq
	// the connector will start consuming from that position
	if position.OptSeq != 0 {
		deliverPolicy = nats.DeliverByStartSequencePolicy
		startSeq = position.OptSeq
	}

	return &nats.ConsumerConfig{
		Durable:        params.Durable,
		ReplayPolicy:   nats.ReplayInstantPolicy,
		DeliverSubject: fmt.Sprintf("%s.%s", params.Durable, params.Stream),
		DeliverPolicy:  deliverPolicy,
		OptStartSeq:    startSeq,
		AckPolicy:      params.AckPolicy,
		FlowControl:    true,
		Heartbeat:      heartbeatTimeout,
	}, nil
}

// canAck checks if a message at the given position can be acknowledged.
func (i *Iterator) canAck(sdkPosition sdk.Position) error {
	if len(i.unackMessages) == 0 {
		return fmt.Errorf("requested ack for %q but no unacknowledged messages found", sdkPosition)
	}

	position, err := i.getMessagePosition(i.unackMessages[0])
	if err != nil {
		return fmt.Errorf("get position: %w", err)
	}

	if bytes.Compare(position, sdkPosition) != 0 {
		return fmt.Errorf(
			"ack is out-of-order, requested ack for %q, but first unack. Message is %q", sdkPosition, position,
		)
	}

	return nil
}

// messageToRecord converts a *nats.Msg to a sdk.Record.
func (i *Iterator) messageToRecord(msg *nats.Msg) (sdk.Record, error) {
	position, err := i.getMessagePosition(msg)
	if err != nil {
		return sdk.Record{}, fmt.Errorf("get position: %w", err)
	}

	// retrieve a message metadata one more time to grab a metadata.Timestamp
	// and use it for a sdk.Record.CreatedAt
	metadata, err := msg.Metadata()
	if err != nil {
		return sdk.Record{}, fmt.Errorf("get message metadata: %w", err)
	}

	if metadata.Timestamp.IsZero() {
		metadata.Timestamp = time.Now()
	}

	return sdk.Record{
		Position:  position,
		CreatedAt: metadata.Timestamp,
		Payload:   sdk.RawData(msg.Data),
	}, nil
}

// getMessagePosition returns a position of a message in the form of sdk.Position.
func (i *Iterator) getMessagePosition(msg *nats.Msg) (sdk.Position, error) {
	metadata, err := msg.Metadata()
	if err != nil {
		return nil, fmt.Errorf("get message metadata: %w", err)
	}

	position := position{
		Durable:   i.consumerInfo.Name,
		Stream:    i.consumerInfo.Stream,
		Subject:   i.subscription.Subject,
		Timestamp: metadata.Timestamp,
		// add 1 to the sequence in order to skip the consumed message at this position
		// and start consuming new messages
		OptSeq: metadata.Sequence.Stream + 1,
	}

	sdkPosition, err := position.marshalSDKPosition()
	if err != nil {
		return nil, fmt.Errorf("marshal sdk position: %w", err)
	}

	return sdkPosition, nil
}
