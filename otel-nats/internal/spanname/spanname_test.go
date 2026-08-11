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
