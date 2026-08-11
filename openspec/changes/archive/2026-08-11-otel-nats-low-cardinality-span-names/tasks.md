# Tasks: otel-nats-low-cardinality-span-names

## 1. Shared destination resolution

- [x] 1.1 Create `otel-nats/internal/spanname` with `Resolve(op, concrete, filter string, inboxPrefixes []string) (name, templateAttr string, inbox bool)` implementing single-filter > concrete precedence, the inbox test on the **resolved** destination, and template-attr emission (design D2b/D3/D5), plus unit tests covering: exact subject, wildcard filter (`*` and `>`), empty filter, empty destination, default-prefix inbox, custom-prefix inbox, unrecognised peer prefix, inbox reached via the filter, and a subject sharing a prefix boundary (`_INBOXES.orders`).

## 2. Inbox prefix resolution (`otelnats/conn.go`)

- [x] 2.1 Add `inboxPrefixes(nc *nats.Conn) []string` returning `nc.Opts.InboxPrefix + "."` (when set) plus `nats.InboxPrefix` unconditionally; resolve once in `newConn` and store on `tracedConn` (design D2c).
- [x] 2.2 Add `inboxAttrs(subject string) []attribute.KeyValue` in `conn_traced.go` returning `messaging.message.conversation_id`, `messaging.destination.temporary=true`, `messaging.destination.anonymous=true`, appended by every inbox-destination span site.

## 3. Core NATS span names (`otelnats/conn_traced.go`)

- [x] 3.1 `startSendSpan`: name via `spanname.Resolve("publish", msg.Subject, "", t.inboxPrefixes)` — renames `send {subject}` → `publish {subject}`; append `messaging.destination.template` when resolution returns one, and `inboxAttrs` when the destination is an inbox (the manual-responder path).
- [x] 3.2 `startRequestSpan`: name `request {dest}` (operation-first, was `{subject} request`), same resolution; update the stale godoc citing "{destination} request".
- [x] 3.3 `recordReply`: span name becomes bare `receive` (no subject); attributes via `inboxAttrs`, applied unconditionally rather than by prefix test — the span is structurally always an inbox.
- [x] 3.4 `wrapMsgHandler`: keep subscription subject as destination, now via `Resolve` with the subscription subject as `filter`; emit `messaging.destination.template` when it differs from `msg.Subject` (wildcard subscriptions), and `inboxAttrs` when the resolved destination is an inbox.

## 4. JetStream span names (`oteljetstream`)

- [x] 4.1 Publish path (`jetstream_traced.go`): `publish {dest}` via `Resolve` with nil inbox prefixes — streams do not capture inbox subjects.
- [x] 4.2 Consumer wrappers: at wrap time read `CachedInfo().Config` `FilterSubject`/`FilterSubjects`, store the single-filter destination (empty string when zero/multiple filters — falls back to concrete).
- [x] 4.3 Receive/process span sites (`consumer_traced.go` ×3, ordered-consumer fallback in `consumer.go`): name via `Resolve(op, msg.Subject(), storedFilter, nil)`; append template attr when produced; ordered/unobservable-config paths pass empty filter.

## 5. Tests

- [x] 5.1 Update every unit-test span-name assertion: `publish {subject}`, `request {subject}`, bare `receive`; add assertions for `messaging.destination.temporary`/`anonymous` on reply-receive and their absence on ordinary publish/process spans (spec scenarios).
- [x] 5.2 New tests: wildcard subscription process span named after subscription subject with template attr; JetStream single-wildcard-filter consumer names `receive {filter}`; multi-filter fallback; publish-to-inbox and inbox-subscription spans named bare; inbox detection via the resolved filter (`<inbox>.>`); a `nats.CustomInboxPrefix` connection recognising both its own and default-prefix inboxes; `_INBOXES.orders` unaffected (spec scenarios).
- [x] 5.3 Update `otel-nats/tests/integration` (and `nats-sampling-e2e` fixtures if they assert span names) for the renames.

## 6. Docs & release

- [x] 6.1 Update `otel-nats` README span tables and repo `CLAUDE.md` NATS notes for new names, inbox behavior, the two-prefix rule, the deliberate absence of a template option, and the `operation.type` / `operation.name` / span-name three-word table.
- [x] 6.2 Bump `instrumentationVersion` to `0.9.0` in `otelnats/conn.go`; write `otel-nats/CHANGELOG.md` entry with BREAKING old→new span-name migration table (design D7).
- [x] 6.3 Run `go build ./...`, `go test -race ./...`, `golangci-lint run ./...` in `otel-nats/` — all green, 0 issues; integration tests with Docker if available.
