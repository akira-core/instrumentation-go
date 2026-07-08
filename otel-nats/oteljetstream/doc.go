// Package oteljetstream provides OpenTelemetry tracing for NATS JetStream.
// It mirrors the API of github.com/nats-io/nats.go/jetstream: New, JetStream, Stream, Consumer.
// Pull consumer management methods are fully wrapped on both
// jetstream.StreamConsumerManager (via JetStream) and jetstream.ConsumerManager
// (via Stream).
//
// Usage aligns with the official package:
//   - New(conn) takes a *otelnats.Conn so that trace is propagated.
//   - Publish and PublishMsg accept context.Context (same as official).
//   - Consume(handler): handler is MsgHandler func(m Msg); m implements Msg (Data, Ack, etc.) and m.Context() carries trace. Naming aligns with otelnats.MsgHandler.
//   - Messages(): Next() returns (ctx, msg, error) with ctx carrying extracted trace.
//   - Next(): returns (ctx, msg, error) for a single message.
//   - Fetch/FetchBytes/FetchNoWait: return MessageBatch; iterate Messages() for Msg (Msg + Context()) with trace per message.
//   - Consumer management methods (e.g. UpdateConsumer, OrderedConsumer, ListConsumers) are exposed and forwarded.
//     Trace-bearing behavior remains in message publish/consume paths.
//   - Push-consumer wrappers (PushConsumer/CreatePushConsumer/CreateOrUpdatePushConsumer/UpdatePushConsumer)
//     are provided on both JetStream and Stream; the returned PushConsumer.Consume carries trace context.
//   - ConsumeContext and Stream fully mirror their jetstream counterparts — no Unwrap needed.
//     Stream's control-plane methods (GetMsg/Purge/PauseConsumer/UnpinConsumer/...) are passthroughs.
//   - Unwrap() on JetStream returns the raw jetstream.JetStream for the KeyValue and
//     ObjectStore APIs (plus AccountInfo/Conn/Options) this messaging wrapper does not re-expose.
//
// Async publish (PublishAsync, PublishMsgAsync) is not wrapped: these APIs take
// no context.Context and return a non-blocking PubAckFuture instead of a
// synchronous ack, which doesn't fit this wrapper's context-propagation model.
package oteljetstream
