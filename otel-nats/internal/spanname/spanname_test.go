package spanname

import "testing"

func TestResolve(t *testing.T) {
	defaultPrefixes := []string{"_INBOX."}
	customPrefixes := []string{"SVCA.", "_INBOX."}

	tests := []struct {
		name          string
		op            string
		concrete      string
		filter        string
		inboxPrefixes []string
		wantName      string
		wantTemplate  string
		wantInbox     bool
	}{
		{
			name:     "concrete only, no filter",
			op:       "publish",
			concrete: "orders.1",
			wantName: "publish orders.1",
		},
		{
			name:     "filter equals concrete",
			op:       "receive",
			concrete: "orders.1",
			filter:   "orders.1",
			wantName: "receive orders.1",
		},
		{
			name:         "filter differs from concrete",
			op:           "receive",
			concrete:     "orders.1",
			filter:       "orders.*",
			wantName:     "receive orders.*",
			wantTemplate: "orders.*",
		},
		{
			name:         "full wildcard filter is a template like any other",
			op:           "process",
			concrete:     "orders.12345",
			filter:       ">",
			wantName:     "process >",
			wantTemplate: ">",
		},
		{
			name:     "degenerate: empty concrete, no filter",
			op:       "receive",
			concrete: "",
			filter:   "",
			wantName: "receive",
		},
		{
			name:          "nil prefixes: inbox subject is named verbatim",
			op:            "publish",
			concrete:      "_INBOX.7Yh2kQ.3",
			inboxPrefixes: nil,
			wantName:      "publish _INBOX.7Yh2kQ.3",
		},
		{
			name:          "default prefix: inbox destination omitted",
			op:            "publish",
			concrete:      "_INBOX.7Yh2kQ.3",
			inboxPrefixes: defaultPrefixes,
			wantName:      "publish",
			wantInbox:     true,
		},
		{
			name:          "default prefix: custom-prefix peer inbox NOT recognised",
			op:            "publish",
			concrete:      "SVCA.7Yh2kQ.3",
			inboxPrefixes: defaultPrefixes,
			wantName:      "publish SVCA.7Yh2kQ.3",
		},
		{
			name:          "custom prefix: own inbox recognised",
			op:            "process",
			concrete:      "SVCA.7Yh2kQ.3",
			inboxPrefixes: customPrefixes,
			wantName:      "process",
			wantInbox:     true,
		},
		{
			name:          "custom prefix: default-prefix peer inbox still recognised",
			op:            "publish",
			concrete:      "_INBOX.7Yh2kQ.3",
			inboxPrefixes: customPrefixes,
			wantName:      "publish",
			wantInbox:     true,
		},
		{
			// The whole reason the test runs on the RESOLVED destination: the filter
			// carries the nuid, so testing concrete alone would still name the span
			// "process _INBOX.7Yh2kQ.>".
			name:          "inbox test applies to the resolved filter, not just concrete",
			op:            "process",
			concrete:      "_INBOX.7Yh2kQ.3",
			filter:        "_INBOX.7Yh2kQ.>",
			inboxPrefixes: defaultPrefixes,
			wantName:      "process",
			wantInbox:     true,
		},
		{
			name:          "inbox never yields a template attribute",
			op:            "receive",
			concrete:      "_INBOX.7Yh2kQ.3",
			filter:        "_INBOX.7Yh2kQ.>",
			inboxPrefixes: defaultPrefixes,
			wantName:      "receive",
			wantTemplate:  "",
			wantInbox:     true,
		},
		{
			// A filter that is nothing but the inbox prefix plus wildcards is a fixed
			// string the subscriber declared: bounded, so semconv rule 1 (use
			// messaging.destination.template when available) applies and the
			// destination stays in the name. The delivery is still an inbox, so the
			// inbox verdict — which drives the temporary/anonymous attributes — holds.
			name:          "prefix-only wildcard filter is bounded and stays in the name",
			op:            "receive",
			concrete:      "_INBOX.7Yh2kQ.3",
			filter:        "_INBOX.>",
			inboxPrefixes: defaultPrefixes,
			wantName:      "receive _INBOX.>",
			wantTemplate:  "_INBOX.>",
			wantInbox:     true,
		},
		{
			name:          "prefix-only single-token wildcard filter is bounded",
			op:            "process",
			concrete:      "_INBOX.7Yh2kQ",
			filter:        "_INBOX.*",
			inboxPrefixes: defaultPrefixes,
			wantName:      "process _INBOX.*",
			wantTemplate:  "_INBOX.*",
			wantInbox:     true,
		},
		{
			name:          "prefix-only multi-token wildcard filter is bounded",
			op:            "receive",
			concrete:      "_INBOX.7Yh2kQ.3",
			filter:        "_INBOX.*.*",
			inboxPrefixes: defaultPrefixes,
			wantName:      "receive _INBOX.*.*",
			wantTemplate:  "_INBOX.*.*",
			wantInbox:     true,
		},
		{
			name:          "prefix-only wildcard filter under a custom prefix is bounded",
			op:            "receive",
			concrete:      "SVCA.7Yh2kQ.3",
			filter:        "SVCA.>",
			inboxPrefixes: customPrefixes,
			wantName:      "receive SVCA.>",
			wantTemplate:  "SVCA.>",
			wantInbox:     true,
		},
		{
			// The bounded carve-out is per matched prefix: the longest matching prefix
			// is stripped, so a custom prefix nested under the default one is measured
			// against its own remainder rather than the default's.
			name:          "longest matching prefix decides the remainder",
			op:            "receive",
			concrete:      "_INBOX.svca.7Yh2kQ.3",
			filter:        "_INBOX.svca.>",
			inboxPrefixes: []string{"_INBOX.svca.", "_INBOX."},
			wantName:      "receive _INBOX.svca.>",
			wantTemplate:  "_INBOX.svca.>",
			wantInbox:     true,
		},
		{
			// One literal token after the prefix is enough to make the filter
			// per-request: this is the "<inbox>.>" subscription shape.
			name:          "a literal token after the prefix keeps the filter unbounded",
			op:            "process",
			concrete:      "_INBOX.7Yh2kQ.3",
			filter:        "_INBOX.7Yh2kQ.*",
			inboxPrefixes: defaultPrefixes,
			wantName:      "process",
			wantInbox:     true,
		},
		{
			// No filter: the concrete inbox subject carries the nuid, so there is no
			// bounded form to keep however the destination was reached.
			name:          "concrete inbox is never bounded",
			op:            "publish",
			concrete:      "_INBOX.7Yh2kQ.3",
			inboxPrefixes: defaultPrefixes,
			wantName:      "publish",
			wantInbox:     true,
		},
		{
			// Degenerate shape: a destination equal to the prefix leaves an empty
			// remainder, which is not a wildcard and must not be treated as bounded.
			name:          "destination equal to the prefix is not bounded",
			op:            "receive",
			concrete:      "_INBOX.",
			inboxPrefixes: defaultPrefixes,
			wantName:      "receive",
			wantInbox:     true,
		},
		{
			name:          "empty destination is not an inbox",
			op:            "receive",
			concrete:      "",
			filter:        "",
			inboxPrefixes: defaultPrefixes,
			wantName:      "receive",
		},
		{
			name:          "empty prefix entries are ignored",
			op:            "publish",
			concrete:      "orders.1",
			inboxPrefixes: []string{"", "_INBOX."},
			wantName:      "publish orders.1",
		},
		{
			name:          "a subject merely sharing a prefix boundary is not an inbox",
			op:            "publish",
			concrete:      "_INBOXES.orders",
			inboxPrefixes: defaultPrefixes,
			wantName:      "publish _INBOXES.orders",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotTemplate, gotInbox := Resolve(tt.op, tt.concrete, tt.filter, tt.inboxPrefixes)
			if gotName != tt.wantName {
				t.Errorf("Resolve() name = %q, want %q", gotName, tt.wantName)
			}
			if gotTemplate != tt.wantTemplate {
				t.Errorf("Resolve() templateAttr = %q, want %q", gotTemplate, tt.wantTemplate)
			}
			if gotInbox != tt.wantInbox {
				t.Errorf("Resolve() inbox = %v, want %v", gotInbox, tt.wantInbox)
			}
		})
	}
}
