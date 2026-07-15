# Tasks: nats-request-conversation-id

## 1. Tests first (RED)

- [x] 1.1 Add `TestRequestReplySpansShareConversationID` to `otel-nats/otelnats/conn_test.go`: instrumented requester + instrumented responder (`conn.Subscribe` wrapper so a process span is emitted; responder uses `msg.Respond`); assert the request "send" span, reply-"receive" span, and responder "process" span all carry `messaging.message.conversation_id` with the same non-empty `_INBOX.`-prefixed value equal to the reply message's subject. Run and watch it fail (attribute absent on all three spans).
- [x] 1.2 Add `TestFailedRequestOmitsConversationID`: `Request` against a subject with no responder (expect error/timeout); assert the "send" span exists, records error status, and carries no `messaging.message.conversation_id`. Watch it fail only if implementation regresses (may pass pre-change — mark as pinning test).
- [x] 1.3 Add fire-and-forget assertion: extend an existing subscribe test (or add one) asserting the "process" span for a reply-less publish carries no `messaging.message.conversation_id`. Pinning test.
- [x] 1.4 Add JetStream exclusion assertion to `otel-nats/oteljetstream` consumer tests: a consumed message (whose native `Reply` is the `$JS.ACK.…` subject) yields a receive/process span with no `messaging.message.conversation_id`. Pinning test.

## 2. Implementation (GREEN)

- [x] 2.1 `otel-nats/otelnats/conn.go` — `receiveAttrs`: add `if msg.Reply != "" { attrs = append(attrs, semconv.MessagingMessageConversationID(msg.Reply)) }` (covers responder "process" spans; reply-receive spans are unaffected by this clause since a reply's own `Reply` field is empty).
- [x] 2.2 `otel-nats/otelnats/conn_traced.go` — `recordReply`: append `semconv.MessagingMessageConversationID(reply.Subject)` to the receive span's start attributes, and `SetAttributes` the same key/value on the request span (late-set; runs before the deferred `reqSpan.End()` in both `requestWithTimeout` and `requestWithCtx`). Update `recordReply`'s doc comment to state the late-set contract and the error-path omission.
- [x] 2.3 `otel-nats/otelnats/conn.go:311` — extend the "keep the attribute sets in sync" comment with the carve-out: `conversation_id` is core-NATS-only; JetStream `Reply` is the `$JS.ACK` subject and must not be mapped.
- [x] 2.4 Run 1.1 test — passes; 1.2/1.3/1.4 pinning tests pass.

## 3. Verification gate

- [x] 3.1 `cd otel-nats && go build ./...` — clean.
- [x] 3.2 `cd otel-nats && go test -race ./...` — all pass.
- [x] 3.3 `cd otel-nats && golangci-lint run ./...` — 0 issues.

## 4. Documentation

- [x] 4.1 `otel-nats/CHANGELOG.md` — add 0.7.0 (Unreleased) **Added** entry: conversation_id on request send (late-set, success only), reply receive, and process spans; JetStream exclusion; note the send-span attribute is invisible to samplers and absent on failed requests.
- [x] 4.2 Repo-root `CLAUDE.md` — if the otel-nats module notes mention span attributes, add a one-line note about the conversation_id contract (send span late-set on success; JetStream excluded). Skip if no natural anchor exists.
