// Package rabbitmq provides a RabbitMQ common
package rabbitmq

import (
	"context"
	"errors"
	"github.com/tx7do/kratos-transport/common"
	"sync"
	"time"

	"github.com/streadway/amqp"
)

type rcommon struct {
	conn           *rabbitMQConn
	addrs          []string
	opts           common.Options
	prefetchCount  int
	prefetchGlobal bool
	mtx            sync.Mutex
	wg             sync.WaitGroup
}

type subscriber struct {
	mtx          sync.Mutex
	mayRun       bool
	opts         common.SubscribeOptions
	topic        string
	ch           *rabbitMQChannel
	durableQueue bool
	queueArgs    map[string]interface{}
	r            *rcommon
	fn           func(msg amqp.Delivery)
	headers      map[string]interface{}
}

type publication struct {
	d   amqp.Delivery
	m   *common.Message
	t   string
	err error
}

func (p *publication) Ack() error {
	return p.d.Ack(false)
}

func (p *publication) Error() error {
	return p.err
}

func (p *publication) Topic() string {
	return p.t
}

func (p *publication) Message() *common.Message {
	return p.m
}

func (s *subscriber) Options() common.SubscribeOptions {
	return s.opts
}

func (s *subscriber) Topic() string {
	return s.topic
}

func (s *subscriber) Unsubscribe() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.mayRun = false
	if s.ch != nil {
		return s.ch.Close()
	}
	return nil
}

func (s *subscriber) resubscribe() {
	minResubscribeDelay := 100 * time.Millisecond
	maxResubscribeDelay := 30 * time.Second
	expFactor := time.Duration(2)
	reSubscribeDelay := minResubscribeDelay
	//loop until unsubscribe
	for {
		s.mtx.Lock()
		mayRun := s.mayRun
		s.mtx.Unlock()
		if !mayRun {
			// we are unsubscribed, showdown routine
			return
		}

		select {
		//check shutdown case
		case <-s.r.conn.close:
			//yep, its shutdown case
			return
			//wait until we reconect to rabbit
		case <-s.r.conn.waitConnection:
		}

		// it may crash (panic) in case of Consume without connection, so recheck it
		s.r.mtx.Lock()
		if !s.r.conn.connected {
			s.r.mtx.Unlock()
			continue
		}

		ch, sub, err := s.r.conn.Consume(
			s.opts.Queue,
			s.topic,
			s.headers,
			s.queueArgs,
			s.opts.AutoAck,
			s.durableQueue,
		)

		s.r.mtx.Unlock()
		switch err {
		case nil:
			reSubscribeDelay = minResubscribeDelay
			s.mtx.Lock()
			s.ch = ch
			s.mtx.Unlock()
		default:
			if reSubscribeDelay > maxResubscribeDelay {
				reSubscribeDelay = maxResubscribeDelay
			}
			time.Sleep(reSubscribeDelay)
			reSubscribeDelay *= expFactor
			continue
		}
		for d := range sub {
			s.r.wg.Add(1)
			s.fn(d)
			s.r.wg.Done()
		}
	}
}

func (r *rcommon) Publish(topic string, msg *common.Message, opts ...common.PublishOption) error {
	m := amqp.Publishing{
		Body:    msg.Body,
		Headers: amqp.Table{},
	}

	options := common.PublishOptions{}
	for _, o := range opts {
		o(&options)
	}

	if options.Context != nil {
		if value, ok := options.Context.Value(deliveryMode{}).(uint8); ok {
			m.DeliveryMode = value
		}

		if value, ok := options.Context.Value(priorityKey{}).(uint8); ok {
			m.Priority = value
		}

		if value, ok := options.Context.Value(contentType{}).(string); ok {
			m.ContentType = value
		}

		if value, ok := options.Context.Value(contentEncoding{}).(string); ok {
			m.ContentEncoding = value
		}

		if value, ok := options.Context.Value(correlationID{}).(string); ok {
			m.CorrelationId = value
		}

		if value, ok := options.Context.Value(replyTo{}).(string); ok {
			m.ReplyTo = value
		}

		if value, ok := options.Context.Value(expiration{}).(string); ok {
			m.Expiration = value
		}

		if value, ok := options.Context.Value(messageID{}).(string); ok {
			m.MessageId = value
		}

		if value, ok := options.Context.Value(timestamp{}).(time.Time); ok {
			m.Timestamp = value
		}

		if value, ok := options.Context.Value(typeMsg{}).(string); ok {
			m.Type = value
		}

		if value, ok := options.Context.Value(userID{}).(string); ok {
			m.UserId = value
		}

		if value, ok := options.Context.Value(appID{}).(string); ok {
			m.AppId = value
		}

	}

	for k, v := range msg.Header {
		m.Headers[k] = v
	}

	if r.conn == nil {
		return errors.New("connection is nil")
	}

	return r.conn.Publish(r.conn.exchange.Name, topic, m)
}

func (r *rcommon) Subscribe(topic string, handler common.Handler, opts ...common.SubscribeOption) (common.Subscriber, error) {
	var ackSuccess bool

	if r.conn == nil {
		return nil, errors.New("not connected")
	}

	opt := common.SubscribeOptions{
		AutoAck: true,
	}

	for _, o := range opts {
		o(&opt)
	}

	// Make sure context is setup
	if opt.Context == nil {
		opt.Context = context.Background()
	}

	ctx := opt.Context
	if subscribeContext, ok := ctx.Value(subscribeContextKey{}).(context.Context); ok && subscribeContext != nil {
		ctx = subscribeContext
	}

	var requeueOnError bool
	requeueOnError, _ = ctx.Value(requeueOnErrorKey{}).(bool)

	var durableQueue bool
	durableQueue, _ = ctx.Value(durableQueueKey{}).(bool)

	var qArgs map[string]interface{}
	if qa, ok := ctx.Value(queueArgumentsKey{}).(map[string]interface{}); ok {
		qArgs = qa
	}

	var headers map[string]interface{}
	if h, ok := ctx.Value(headersKey{}).(map[string]interface{}); ok {
		headers = h
	}

	if bval, ok := ctx.Value(ackSuccessKey{}).(bool); ok && bval {
		opt.AutoAck = false
		ackSuccess = true
	}

	fn := func(msg amqp.Delivery) {
		header := make(map[string]string)
		for k, v := range msg.Headers {
			header[k], _ = v.(string)
		}
		m := &common.Message{
			Header: header,
			Body:   msg.Body,
		}
		p := &publication{d: msg, m: m, t: msg.RoutingKey}
		p.err = handler(p)
		if p.err == nil && ackSuccess && !opt.AutoAck {
			_ = msg.Ack(false)
		} else if p.err != nil && !opt.AutoAck {
			_ = msg.Nack(false, requeueOnError)
		}
	}

	sub := &subscriber{topic: topic, opts: opt, mayRun: true, r: r,
		durableQueue: durableQueue, fn: fn, headers: headers, queueArgs: qArgs}

	go sub.resubscribe()

	return sub, nil
}

func (r *rcommon) Options() common.Options {
	return r.opts
}

func (r *rcommon) String() string {
	return "rabbitmq"
}

func (r *rcommon) Address() string {
	if len(r.addrs) > 0 {
		return r.addrs[0]
	}
	return ""
}

func (r *rcommon) Init(opts ...common.Option) error {
	for _, o := range opts {
		o(&r.opts)
	}
	r.addrs = r.opts.Addrs
	return nil
}

func (r *rcommon) Connect() error {
	if r.conn == nil {
		r.conn = newRabbitMQConn(r.getExchange(), r.opts.Addrs, r.getPrefetchCount(), r.getPrefetchGlobal())
	}

	conf := defaultAmqpConfig

	if auth, ok := r.opts.Context.Value(externalAuth{}).(ExternalAuthentication); ok {
		conf.SASL = []amqp.Authentication{&auth}
	}

	conf.TLSClientConfig = r.opts.TLSConfig

	return r.conn.Connect(r.opts.Secure, &conf)
}

func (r *rcommon) Disconnect() error {
	if r.conn == nil {
		return errors.New("connection is nil")
	}
	ret := r.conn.Close()
	r.wg.Wait() // wait all goroutines
	return ret
}

func NewBroker(opts ...common.Option) common.Broker {
	options := common.Options{
		Context: context.Background(),
	}

	for _, o := range opts {
		o(&options)
	}

	return &rcommon{
		addrs: options.Addrs,
		opts:  options,
	}
}

func (r *rcommon) getExchange() Exchange {

	ex := DefaultExchange

	if e, ok := r.opts.Context.Value(exchangeKey{}).(string); ok {
		ex.Name = e
	}

	if d, ok := r.opts.Context.Value(durableExchange{}).(bool); ok {
		ex.Durable = d
	}

	return ex
}

func (r *rcommon) getPrefetchCount() int {
	if e, ok := r.opts.Context.Value(prefetchCountKey{}).(int); ok {
		return e
	}
	return DefaultPrefetchCount
}

func (r *rcommon) getPrefetchGlobal() bool {
	if e, ok := r.opts.Context.Value(prefetchGlobalKey{}).(bool); ok {
		return e
	}
	return DefaultPrefetchGlobal
}