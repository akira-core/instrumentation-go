package otelmongo

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// TestFlagMatrix_AllCombinations is the integration-level companion to the
// gate truth-table tests in env_flags_test.go. For every one of the 8
// possible (global × module-tracing × module-propagation) settings it
// asserts:
//
//  1. NewCollection picks the correct strategy impl (direct vs traced).
//     This is the structural enforcement of the disabled-mode invariant —
//     when tracing is off no traced.Collection ever exists, so no OTel SDK
//     span can be created from the wrapper.
//  2. When the traced impl IS selected, its PropagationEnabled bit matches
//     the resolved three-tier gate, locking in that no _oteltrace is
//     injected unless all three gates agree.
//  3. The cached propagation gate (cachedPropagationEnabled) reflects the
//     same three-tier resolution — this is what ContextFromDocument /
//     ContextFromRawDocument consult on every change-stream tick, so it
//     MUST stay coherent with the per-Collection decision.
//
// This is the single test the disabled-mode contract relies on for v1.
func TestFlagMatrix_AllCombinations(t *testing.T) {
	type want struct {
		wantTraced         bool
		wantPropagationBit bool
		wantCachedProp     bool
	}
	cases := []struct {
		name    string
		global  string // "" = unset
		tracing string
		prop    string
		want    want
	}{
		{"all_unset", "", "", "", want{false, false, false}},
		{"global_on_only", "1", "", "", want{false, false, false}},
		{"tracing_on_only", "", "1", "", want{false, false, false}},
		{"prop_on_only", "", "", "1", want{false, false, false}},
		{"global_off_others_on", "0", "1", "1", want{false, false, false}},
		{"global_on_tracing_off_prop_on", "1", "0", "1", want{false, false, false}},
		{"global_on_tracing_on_prop_off", "1", "1", "0", want{true, false, false}},
		{"all_on", "1", "1", "1", want{true, true, true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyEnv(t, envGlobalTracingEnabled, tc.global)
			applyEnv(t, envMongoTracingEnabled, tc.tracing)
			applyEnv(t, envMongoPropagationEnabled, tc.prop)
			resetPropEnabledCacheForTest()
			t.Cleanup(resetPropEnabledCacheForTest)

			coll := NewCollection(nil, otel.Tracer("test"), otel.GetTextMapPropagator())
			require.NotNil(t, coll)

			if tc.want.wantTraced {
				tc2, ok := coll.impl.(*traced.Collection)
				require.True(t, ok, "want traced.Collection, got %T", coll.impl)
				assert.Equal(t, tc.want.wantPropagationBit, tc2.PropagationEnabled,
					"PropagationEnabled bit must mirror the three-tier resolution")
			} else {
				_, ok := coll.impl.(*direct.Collection)
				require.True(t, ok, "want direct.Collection (no OTel SDK reachable), got %T", coll.impl)
			}

			assert.Equal(t, tc.want.wantCachedProp, cachedPropagationEnabled(),
				"cachedPropagationEnabled() must reflect the same three-tier resolution")
		})
	}
}

// TestFlagMatrix_ContextFromDocumentReturnsZeroWhenGated verifies the
// package-level ContextFromDocument honors the cached propagation gate for
// every disabled combination. A document carrying a valid _oteltrace field
// MUST yield ok=false whenever any of the three gates is off — even though
// the document content itself would parse to a valid SpanContext.
func TestFlagMatrix_ContextFromDocumentReturnsZeroWhenGated(t *testing.T) {
	disabledCases := []struct {
		name    string
		global  string
		tracing string
		prop    string
	}{
		{"all_unset", "", "", ""},
		{"global_off", "0", "1", "1"},
		{"tracing_off", "1", "0", "1"},
		{"prop_off", "1", "1", "0"},
	}
	doc := map[string]any{
		"_oteltrace": map[string]any{
			"traceparent": "00-12345678901234567890123456789012-0123456789012345-01",
		},
	}
	for _, tc := range disabledCases {
		t.Run(tc.name, func(t *testing.T) {
			applyEnv(t, envGlobalTracingEnabled, tc.global)
			applyEnv(t, envMongoTracingEnabled, tc.tracing)
			applyEnv(t, envMongoPropagationEnabled, tc.prop)
			resetPropEnabledCacheForTest()
			t.Cleanup(resetPropEnabledCacheForTest)

			sc, ok := ContextFromDocument(t.Context(), doc)
			assert.False(t, ok, "any gate off → ContextFromDocument must return ok=false")
			assert.False(t, sc.IsValid(), "any gate off → returned SpanContext must be invalid")
		})
	}

	t.Run("all_on_extracts", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "1")
		t.Setenv(envMongoPropagationEnabled, "1")
		resetPropEnabledCacheForTest()
		t.Cleanup(resetPropEnabledCacheForTest)

		sc, ok := ContextFromDocument(t.Context(), doc)
		require.True(t, ok, "all gates on → extraction must succeed")
		assert.True(t, sc.IsValid())
		assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
	})
}

// applyEnv either sets or unsets the env var depending on whether the value
// is the empty string. Mirrors the pattern used in env_flags_test.go.
func applyEnv(t *testing.T, key, value string) {
	t.Helper()
	if value == "" {
		_ = os.Unsetenv(key)
		return
	}
	t.Setenv(key, value)
}
