package otelmongo

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// TestFlagMatrix_AllCombinationsV2 — see v1 sibling for full contract notes.
// Asserts NewCollection picks the right strategy impl AND that the cached
// propagation gate stays coherent across every combination.
func TestFlagMatrix_AllCombinationsV2(t *testing.T) {
	type want struct {
		wantTraced         bool
		wantPropagationBit bool
		wantCachedProp     bool
	}
	cases := []struct {
		name    string
		global  string
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
			applyEnvV2(t, envGlobalTracingEnabled, tc.global)
			applyEnvV2(t, envMongoTracingEnabled, tc.tracing)
			applyEnvV2(t, envMongoPropagationEnabled, tc.prop)
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

func TestFlagMatrix_ContextFromDocumentReturnsZeroWhenGatedV2(t *testing.T) {
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
			applyEnvV2(t, envGlobalTracingEnabled, tc.global)
			applyEnvV2(t, envMongoTracingEnabled, tc.tracing)
			applyEnvV2(t, envMongoPropagationEnabled, tc.prop)
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
		require.True(t, ok)
		assert.True(t, sc.IsValid())
		assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
	})
}

func applyEnvV2(t *testing.T, key, value string) {
	t.Helper()
	if value == "" {
		_ = os.Unsetenv(key)
		return
	}
	t.Setenv(key, value)
}
