package amqp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/inturn/kit/endpoint"
	"github.com/inturn/kit/log"
	"github.com/streadway/amqp"
)

// Subscriber wraps an endpoint and provides a handler for AMQP Delivery messages.
type Subscriber struct {
	e            endpoint.Endpoint
	dec          DecodeRequestFunc
	enc          EncodeResponseFunc
	before       []RequestFunc
	after        []SubscriberResponseFunc
	finalizer    []SubscriberFinalizerFunc
	errorEncoder ErrorEncoder
	logger       log.Logger
}

// NewSubscriber constructs a new subscriber, which provides a handler
// for AMQP Delivery messages.
func NewSubscriber(
	e endpoint.Endpoint,
	dec DecodeRequestFunc,
	enc EncodeResponseFunc,
	options ...SubscriberOption,
) *Subscriber {
	s := &Subscriber{
		e:            e,
		dec:          dec,
		enc:          enc,
		errorEncoder: DefaultErrorEncoder,
		logger:       log.NewNopLogger(),
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// SubscriberOption sets an optional parameter for subscribers.
type SubscriberOption func(*Subscriber)

// SubscriberBefore functions are executed on the publisher delivery object
// before the request is decoded.
func SubscriberBefore(before ...RequestFunc) SubscriberOption {
	return func(s *Subscriber) { s.before = append(s.before, before...) }
}

// SubscriberAfter functions are executed on the subscriber reply after the
// endpoint is invoked, but before anything is published to the reply.
func SubscriberAfter(after ...SubscriberResponseFunc) SubscriberOption {
	return func(s *Subscriber) { s.after = append(s.after, after...) }
}

// SubscriberErrorEncoder is used to encode errors to the subscriber reply
// whenever they're encountered in the processing of a request. Clients can
// use this to provide custom error formatting. By default,
// errors will be published with the DefaultErrorEncoder.
func SubscriberErrorEncoder(ee ErrorEncoder) SubscriberOption {
	return func(s *Subscriber) { s.errorEncoder = ee }
}

// SubscriberErrorLogger is used to log non-terminal errors. By default, no errors
// are logged. This is intended as a diagnostic measure. Finer-grained control
// of error handling, including logging in more detail, should be performed in a
// custom SubscriberErrorEncoder which has access to the context.
func SubscriberErrorLogger(logger log.Logger) SubscriberOption {
	return func(s *Subscriber) { s.logger = logger }
}

// ServerFinalizer is executed at the end of every MQ request.
// By default, no finalizer is registered.
func ServerFinalizer(f ...SubscriberFinalizerFunc) SubscriberOption {
	return func(s *Subscriber) { s.finalizer = append(s.finalizer, f...) }
}

// ServeDelivery handles AMQP Delivery messages
// It is strongly recommended to use *amqp.Channel as the
// Channel interface implementation.
func (s Subscriber) ServeDelivery(ch Channel) func(deliv *amqp.Delivery) {
	return func(deliv *amqp.Delivery) {
		ctx, cancel := context.WithCancel(context.Background())
		var err error
		defer cancel()

		if len(s.finalizer) > 0 {
			defer func() {
				for _, f := range s.finalizer {
					f(ctx, err)
				}
			}()
		}

		pub := amqp.Publishing{}

		for _, f := range s.before {
			ctx = f(ctx, &pub, deliv)
		}

		request, err := s.dec(ctx, deliv)
		if err != nil {
			s.logger.Log("err", err)
			s.errorEncoder(ctx, err, deliv, ch, &pub)
			return
		}

		response, err := s.e(ctx, request)
		if err != nil {
			s.logger.Log("err", err)
			s.errorEncoder(ctx, err, deliv, ch, &pub)
			return
		}

		for _, f := range s.after {
			ctx = f(ctx, deliv, ch, &pub)
		}

		if err = s.enc(ctx, &pub, response); err != nil {
			s.logger.Log("err", err)
			s.errorEncoder(ctx, err, deliv, ch, &pub)
			return
		}

		if err = s.publishResponse(ctx, deliv, ch, &pub); err != nil {
			s.logger.Log("err", err)
			s.errorEncoder(ctx, err, deliv, ch, &pub)
			return
		}
	}

}

func (s Subscriber) publishResponse(
	ctx context.Context,
	deliv *amqp.Delivery,
	ch Channel,
	pub *amqp.Publishing,
) error {
	if pub.CorrelationId == "" {
		pub.CorrelationId = deliv.CorrelationId
	}

	replyExchange := getPublishExchange(ctx)
	replyTo := getPublishKey(ctx)
	if replyTo == "" {
		replyTo = deliv.ReplyTo
	}

	return ch.Publish(
		replyExchange,
		replyTo,
		false, // mandatory
		false, // immediate
		*pub,
	)
}

// EncodeJSONResponse marshals the response as JSON as part of the
// payload of the AMQP Publishing object.
func EncodeJSONResponse(
	ctx context.Context,
	pub *amqp.Publishing,
	response interface{},
) error {
	b, err := json.Marshal(response)
	if err != nil {
		return err
	}
	pub.Body = b
	return nil
}

// EncodeNopResponse is a response function that does nothing.
func EncodeNopResponse(
	ctx context.Context,
	pub *amqp.Publishing,
	response interface{},
) error {
	return nil
}

// ErrorEncoder is responsible for encoding an error to the subscriber reply.
// Users are encouraged to use custom ErrorEncoders to encode errors to
// their replies, and will likely want to pass and check for their own error
// types.
type ErrorEncoder func(ctx context.Context,
	err error, deliv *amqp.Delivery, ch Channel, pub *amqp.Publishing)

// DefaultErrorEncoder simply ignores the message. It does not reply
// nor Ack/Nack the message.
func DefaultErrorEncoder(ctx context.Context,
	err error, deliv *amqp.Delivery, ch Channel, pub *amqp.Publishing) {
}

// SingleNackRequeueErrorEncoder issues a Nack to the delivery with multiple flag set as false
// and requeue flag set as true. It does not reply the message.
func SingleNackRequeueErrorEncoder(ctx context.Context,
	err error, deliv *amqp.Delivery, ch Channel, pub *amqp.Publishing) {
	deliv.Nack(
		false, //multiple
		true,  //requeue
	)
	duration := getNackSleepDuration(ctx)
	time.Sleep(duration)
}

// ReplyErrorEncoder serializes the error message as a DefaultErrorResponse
// JSON and sends the message to the ReplyTo address.
func ReplyErrorEncoder(
	ctx context.Context,
	err error,
	deliv *amqp.Delivery,
	ch Channel,
	pub *amqp.Publishing,
) {

	if pub.CorrelationId == "" {
		pub.CorrelationId = deliv.CorrelationId
	}

	replyExchange := getPublishExchange(ctx)
	replyTo := getPublishKey(ctx)
	if replyTo == "" {
		replyTo = deliv.ReplyTo
	}

	response := DefaultErrorResponse{err.Error()}

	b, err := json.Marshal(response)
	if err != nil {
		return
	}
	pub.Body = b

	ch.Publish(
		replyExchange,
		replyTo,
		false, // mandatory
		false, // immediate
		*pub,
	)
}

// ReplyAndAckErrorEncoder serializes the error message as a DefaultErrorResponse
// JSON and sends the message to the ReplyTo address then Acks the original
// message.
func ReplyAndAckErrorEncoder(ctx context.Context, err error, deliv *amqp.Delivery, ch Channel, pub *amqp.Publishing) {
	ReplyErrorEncoder(ctx, err, deliv, ch, pub)
	deliv.Ack(false)
}

// DefaultErrorResponse is the default structure of responses in the event
// of an error.
type DefaultErrorResponse struct {
	Error string `json:"err"`
}

// ServerFinalizerFunc can be used to perform work at the end of an MQ
// request, after the response has been written to the client. The principal
// intended use is for request logging. In addition to the response code
// provided in the function signature, additional response parameters are
// provided in the context under keys with the ContextKeyResponse prefix.
type SubscriberFinalizerFunc func(ctx context.Context, err error)

// Channel is a channel interface to make testing possible.
// It is highly recommended to use *amqp.Channel as the interface implementation.
type Channel interface {
	Publish(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
	Consume(queue, consumer string, autoAck, exclusive, noLocal, noWail bool, args amqp.Table) (<-chan amqp.Delivery, error)
}