// (c) 2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package stream

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	avlancheGoUtils "github.com/ava-labs/avalanchego/utils"

	"github.com/ava-labs/ortelius/services/db"

	"github.com/ava-labs/ortelius/services/metrics"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/segmentio/kafka-go"

	"github.com/ava-labs/ortelius/cfg"
	"github.com/ava-labs/ortelius/services"
)

const (
	kafkaReadTimeout = 10 * time.Second

	ConsumerEventTypeDefault = EventTypeDecisions
	ConsumerMaxBytesDefault  = 10e8

	pollLimit = 5000
	pollSleep = 250 * time.Millisecond

	defaultTxChannelSize = 1024

	// DefaultConsumeProcessWriteTimeout consume context
	DefaultConsumeProcessWriteTimeout = 3 * time.Minute
)

type serviceConsumerFactory func(uint32, string, string) (services.Consumer, error)

// consumer takes events from Kafka and sends them to a service consumer
type consumer struct {
	id        string
	eventType EventType

	chainID  string
	reader   *kafka.Reader
	consumer services.Consumer
	conns    *services.Connections
	sc       *services.Control

	// metrics
	metricProcessedCountKey       string
	metricFailureCountKey         string
	metricProcessMillisCounterKey string
	metricSuccessCountKey         string

	groupName string
	topicName string

	idx    int
	maxIdx int

	processorDoneCh chan struct{}
	queueCnt        int64
	txChannel       chan *TransactionWorkPacket
}

// NewConsumerFactory returns a processorFactory for the given service consumer
func NewConsumerFactory(factory serviceConsumerFactory, eventType EventType) ProcessorFactory {
	return func(sc *services.Control, conf cfg.Config, chainVM string, chainID string, idx int, maxIdx int) (Processor, error) {
		conns, err := sc.DatabaseOnly()
		if err != nil {
			return nil, err
		}

		c := &consumer{
			eventType: eventType,
			idx:       idx,
			maxIdx:    maxIdx,
			chainID:   chainID,
			conns:     conns,
			sc:        sc,

			processorDoneCh: make(chan struct{}),
		}

		switch eventType {
		case EventTypeDecisions:
			c.metricProcessedCountKey = fmt.Sprintf("consume_records_processed_%s", chainID)
			c.metricProcessMillisCounterKey = fmt.Sprintf("consume_records_process_millis_%s", chainID)
			c.metricSuccessCountKey = fmt.Sprintf("consume_records_success_%s", chainID)
			c.metricFailureCountKey = fmt.Sprintf("consume_records_failure_%s", chainID)
			c.id = fmt.Sprintf("consumer %d %s %s", conf.NetworkID, chainVM, chainID)
		case EventTypeConsensus:
			c.metricProcessedCountKey = fmt.Sprintf("consume_consensus_records_processed_%s", chainID)
			c.metricProcessMillisCounterKey = fmt.Sprintf("consume_consensus_records_process_millis_%s", chainID)
			c.metricSuccessCountKey = fmt.Sprintf("consume_consensus_records_success_%s", chainID)
			c.metricFailureCountKey = fmt.Sprintf("consume_consensus_records_failure_%s", chainID)
			c.id = fmt.Sprintf("consumer_consensus %d %s %s", conf.NetworkID, chainVM, chainID)
		}

		metrics.Prometheus.CounterInit(c.metricProcessedCountKey, "records processed")
		metrics.Prometheus.CounterInit(c.metricProcessMillisCounterKey, "records processed millis")
		metrics.Prometheus.CounterInit(c.metricSuccessCountKey, "records success")
		metrics.Prometheus.CounterInit(c.metricFailureCountKey, "records failure")
		sc.InitConsumeMetrics()

		// Create consumer backend
		c.consumer, err = factory(conf.NetworkID, chainVM, chainID)
		if err != nil {
			c.Close()
			return nil, err
		}

		// Setup config
		c.groupName = conf.Consumer.GroupName
		if c.groupName == "" {
			c.groupName = c.consumer.Name()
		}
		if !conf.Consumer.StartTime.IsZero() {
			c.groupName = ""
		}

		c.topicName = GetTopicName(conf.NetworkID, chainID, c.eventType)
		// Create reader for the topic
		c.reader = kafka.NewReader(kafka.ReaderConfig{
			Topic:       c.topicName,
			Brokers:     conf.Kafka.Brokers,
			GroupID:     c.groupName,
			StartOffset: kafka.FirstOffset,
			MaxBytes:    ConsumerMaxBytesDefault,
		})

		// If the start time is set then seek to the correct offset
		if !conf.Consumer.StartTime.IsZero() {
			ctx, cancelFn := context.WithTimeout(context.Background(), kafkaReadTimeout)
			defer cancelFn()

			if err = c.reader.SetOffsetAt(ctx, conf.Consumer.StartTime); err != nil {
				c.Close()
				return nil, err
			}
		}

		c.txChannel = make(chan *TransactionWorkPacket, defaultTxChannelSize)
		for ipos := 0; ipos <= c.maxIdx; ipos++ {
			conns, err := c.sc.DatabaseOnly()
			if err != nil {
				return nil, err
			}
			go c.transactionProcessor(conns, c.txChannel)
		}

		return c, nil
	}
}

func (c *consumer) ID() string {
	return c.id
}

// Close closes the consumer
func (c *consumer) Close() error {
	c.sc.Log.Info("close %s", c.id)
	errs := wrappers.Errs{}
	if c.reader != nil {
		errs.Add(c.reader.Close())
	}
	close(c.processorDoneCh)
	if c.conns != nil {
		errs.Add(c.conns.Close())
	}
	return errs.Err
}

// ProcessNextMessage waits for a new Message and adds it to the services
func (c *consumer) ProcessNextMessage() error {
	wm := func(msg *Message) error {
		var err error
		collectors := metrics.NewCollectors(
			metrics.NewCounterIncCollect(c.metricProcessedCountKey),
			metrics.NewCounterObserveMillisCollect(c.metricProcessMillisCounterKey),
			metrics.NewCounterIncCollect(services.MetricConsumeProcessedCountKey),
			metrics.NewCounterObserveMillisCollect(services.MetricConsumeProcessMillisCounterKey),
		)
		defer func() {
			err := collectors.Collect()
			if err != nil {
				c.sc.Log.Error("collectors.Collect: %s", err)
			}
		}()

		for {
			err = c.persistConsume(msg)
			if !db.ErrIsLockError(err) {
				break
			}
		}
		if err != nil {
			collectors.Error()
			c.sc.Log.Error("consumer.Consume: %s", err)
			return err
		}

		c.sc.BalanceAccumulatorManager.Run(c.sc)
		return err
	}

	if c.sc.IsDBPoll {
		sess := c.conns.DB().NewSessionForEventReceiver(c.conns.QuietStream().NewJob("query-txpoll"))
		if true {
			var err error
			var rowdata []*services.TxPool
			rowdata, err = fetchPollForTopic(sess, c.topicName, nil, c.maxIdx)

			if err != nil {
				return err
			}

			if len(rowdata) == 0 {
				time.Sleep(pollSleep)
				return nil
			}

			errs := &avlancheGoUtils.AtomicInterface{}

			for _, row := range rowdata {
				msg := &Message{
					id:         row.MsgKey,
					chainID:    c.chainID,
					body:       row.Serialization,
					timestamp:  row.CreatedAt.UTC().Unix(),
					nanosecond: int64(row.CreatedAt.UTC().Nanosecond()),
				}
				atomic.AddInt64(&c.queueCnt, 1)
				c.txChannel <- &TransactionWorkPacket{msg: msg, row: row, errs: errs}
			}

			for atomic.LoadInt64(&c.queueCnt) > 0 {
				switch {
				case errs.GetValue() != nil:
					return errs.GetValue().(error)
				default:
					time.Sleep(time.Millisecond)
				}
			}

			return nil
		}

		updateStatus := func(txPoll *services.TxPool) error {
			ctx, cancelFn := context.WithTimeout(context.Background(), DefaultConsumeProcessWriteTimeout)
			defer cancelFn()
			return c.sc.Persist.UpdateTxPoolStatus(ctx, sess, txPoll)
		}

		var err error
		var rowdata []*services.TxPool
		rowdata, err = fetchPollForTopic(sess, c.topicName, &c.idx, c.maxIdx)

		if err != nil {
			return err
		}

		if len(rowdata) == 0 {
			time.Sleep(pollSleep)
			return nil
		}

		for _, row := range rowdata {
			msg := &Message{
				id:         row.MsgKey,
				chainID:    c.chainID,
				body:       row.Serialization,
				timestamp:  row.CreatedAt.UTC().Unix(),
				nanosecond: int64(row.CreatedAt.UTC().Nanosecond()),
			}
			err = wm(msg)
			if err != nil {
				return err
			}
			row.Processed = 1
			err = updateStatus(row)
			if err != nil {
				return err
			}
		}

		return nil
	}

	msg, err := c.nextMessage()
	if err != nil {
		if err != context.DeadlineExceeded {
			c.sc.Log.Error("consumer.getNextMessage: %s", err.Error())
		}
		return err
	}

	err = wm(msg)
	if err != nil {
		return err
	}

	return c.commitMessage(msg)
}

func (c *consumer) persistConsume(msg *Message) error {
	ctx, cancelFn := context.WithTimeout(context.Background(), DefaultConsumeProcessWriteTimeout)
	defer cancelFn()
	switch c.eventType {
	case EventTypeDecisions:
		return c.consumer.Consume(ctx, c.conns, msg, c.sc.Persist)
	case EventTypeConsensus:
		return c.consumer.ConsumeConsensus(ctx, c.conns, msg, c.sc.Persist)
	default:
		return fmt.Errorf("invalid eventType %v", c.eventType)
	}
}

func (c *consumer) nextMessage() (*Message, error) {
	ctx, cancelFn := context.WithTimeout(context.Background(), kafkaReadTimeout)
	defer cancelFn()

	return c.getNextMessage(ctx)
}

func (c *consumer) Failure() {
	_ = metrics.Prometheus.CounterInc(c.metricFailureCountKey)
	_ = metrics.Prometheus.CounterInc(services.MetricConsumeFailureCountKey)
}

func (c *consumer) Success() {
	_ = metrics.Prometheus.CounterInc(c.metricSuccessCountKey)
	_ = metrics.Prometheus.CounterInc(services.MetricConsumeSuccessCountKey)
}

func (c *consumer) commitMessage(msg services.Consumable) error {
	ctx, cancelFn := context.WithTimeout(context.Background(), kafkaReadTimeout)
	defer cancelFn()
	return c.reader.CommitMessages(ctx, *msg.KafkaMessage())
}

// getNextMessage gets the next Message from the Kafka Indexer
func (c *consumer) getNextMessage(ctx context.Context) (*Message, error) {
	// Get raw Message from Kafka
	msg, err := c.reader.FetchMessage(ctx)
	if err != nil {
		return nil, err
	}

	m := &Message{
		chainID:      c.chainID,
		body:         msg.Value,
		timestamp:    msg.Time.UTC().Unix(),
		nanosecond:   int64(msg.Time.UTC().Nanosecond()),
		kafkaMessage: &msg,
	}
	// Extract Message ID from key
	id, err := ids.ToID(msg.Key)
	if err != nil {
		m.id = string(msg.Key)
	} else {
		m.id = id.String()
	}

	return m, nil
}

type TransactionWorkPacket struct {
	msg  *Message
	row  *services.TxPool
	errs *avlancheGoUtils.AtomicInterface
}

func (c *consumer) transactionProcessor(conns *services.Connections, que <-chan *TransactionWorkPacket) {
	defer func() {
		_ = conns.Close()
	}()

	for {
		select {
		case w := <-que:
			err := c.doUpdates(w.msg, w.row)
			if err != nil {
				w.errs.SetValue(err)
				return
			}
			atomic.AddInt64(&c.queueCnt, -1)
		case <-c.processorDoneCh:
			return
		}
	}
}

func (c *consumer) doUpdates(msgval *Message, row *services.TxPool) error {
	wm := func(msg *Message) error {
		var err error
		collectors := metrics.NewCollectors(
			metrics.NewCounterIncCollect(c.metricProcessedCountKey),
			metrics.NewCounterObserveMillisCollect(c.metricProcessMillisCounterKey),
			metrics.NewCounterIncCollect(services.MetricConsumeProcessedCountKey),
			metrics.NewCounterObserveMillisCollect(services.MetricConsumeProcessMillisCounterKey),
		)
		defer func() {
			err := collectors.Collect()
			if err != nil {
				c.sc.Log.Error("collectors.Collect: %s", err)
			}
		}()

		for {
			err = c.persistConsume(msg)
			if !db.ErrIsLockError(err) {
				break
			}
		}
		if err != nil {
			collectors.Error()
			c.sc.Log.Error("consumer.Consume: %s", err)
			return err
		}

		c.sc.BalanceAccumulatorManager.Run(c.sc)
		return err
	}

	updateStatus := func(txPoll *services.TxPool) error {
		sess := c.conns.DB().NewSessionForEventReceiver(c.conns.QuietStream().NewJob("consumer-work"))
		ctx, cancelFn := context.WithTimeout(context.Background(), DefaultConsumeProcessWriteTimeout)
		defer cancelFn()
		return c.sc.Persist.UpdateTxPoolStatus(ctx, sess, txPoll)
	}

	err := wm(msgval)
	if err != nil {
		return err
	}

	row.Processed = 1
	return updateStatus(row)
}
