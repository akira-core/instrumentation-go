package shared

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ctxWithSampledSpan returns a context carrying a recording span so
// traceMetadataFromContext returns valid metadata. Required for M1 tests that
// exercise the inject path; without a valid span context, fast paths still
// clone but never append the _oteltrace entry.
func ctxWithSampledSpan(t *testing.T) (context.Context, propagation.TextMapPropagator) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("m1-test").Start(context.Background(), "root")
	t.Cleanup(func() { span.End() })
	return ctx, propagation.TraceContext{}
}

// TestInjectDoesNotMutateOriginalBsonD verifies the M1 fast path for bson.D
// inputs returns a fresh slice and never mutates the caller's value.
func TestInjectDoesNotMutateOriginalBsonD(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	orig := bson.D{{Key: "name", Value: "alice"}, {Key: "age", Value: 30}}
	origCopy := bson.D{{Key: "name", Value: "alice"}, {Key: "age", Value: 30}}

	out, err := InjectTraceIntoDocument(ctx, orig, prop)
	require.NoError(t, err)

	// Caller's original must be untouched (length + content).
	require.Len(t, orig, len(origCopy), "caller bson.D length must not change")
	for i := range orig {
		assert.Equal(t, origCopy[i].Key, orig[i].Key)
		assert.Equal(t, origCopy[i].Value, orig[i].Value)
	}
	// Output must contain the _oteltrace entry at the tail.
	require.Greater(t, len(out), len(orig))
	assert.Equal(t, TraceMetadataKey, out[len(out)-1].Key)
}

// TestInjectDoesNotShareBackingArray ensures appending to the caller's slice
// after Inject returns cannot corrupt the returned slice — same-backing-array
// bugs only surface when caller appends past their original length.
func TestInjectDoesNotShareBackingArray(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	orig := make(bson.D, 0, 10) // generous capacity so append below stays in same array
	orig = append(orig, bson.E{Key: "name", Value: "alice"})

	out, err := InjectTraceIntoDocument(ctx, orig, prop)
	require.NoError(t, err)

	// Capture the value at out[1] (should be _oteltrace).
	require.Equal(t, TraceMetadataKey, out[1].Key, "expected _oteltrace at position 1")

	// Caller appends two more entries to their original slice. If backing array
	// was shared, this overwrites out[1] (the _oteltrace entry).
	_ = append(orig, bson.E{Key: "intruder1", Value: "X"})
	_ = append(orig, bson.E{Key: "intruder2", Value: "Y"})

	// out must still hold _oteltrace untouched.
	assert.Equal(t, TraceMetadataKey, out[1].Key, "backing array must not be shared with caller")
}

// TestInjectDoesNotMutateOriginalBsonM verifies the M1 fast path for bson.M
// inputs never writes back to the caller's map.
func TestInjectDoesNotMutateOriginalBsonM(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	orig := bson.M{"name": "alice", "age": 30}

	out, err := InjectTraceIntoDocument(ctx, orig, prop)
	require.NoError(t, err)

	// Caller's map must not have _oteltrace.
	_, exists := orig[TraceMetadataKey]
	assert.False(t, exists, "caller bson.M must not be mutated")
	assert.Len(t, orig, 2, "caller bson.M length must not change")

	// Output is bson.D with _oteltrace appended.
	require.NotEmpty(t, out)
	assert.Equal(t, TraceMetadataKey, out[len(out)-1].Key)
}

// TestInjectDoesNotMutateOriginalMap verifies the M1 fast path for the
// concrete map[string]any input alias also clones the caller map.
func TestInjectDoesNotMutateOriginalMap(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	orig := map[string]any{"name": "alice", "age": 30}

	out, err := InjectTraceIntoDocument(ctx, orig, prop)
	require.NoError(t, err)

	_, exists := orig[TraceMetadataKey]
	assert.False(t, exists, "caller map[string]any must not be mutated")
	assert.Len(t, orig, 2)
	assert.Equal(t, TraceMetadataKey, out[len(out)-1].Key)
}

// TestInjectStructFallbackBehaviorUnchanged ensures struct inputs still go
// through the original Marshal/Unmarshal path and produce equivalent output.
func TestInjectStructFallbackBehaviorUnchanged(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	type sample struct {
		Name string `bson:"name"`
		Age  int    `bson:"age"`
	}
	doc := sample{Name: "alice", Age: 30}

	out, err := InjectTraceIntoDocument(ctx, doc, prop)
	require.NoError(t, err)

	// Output must be a bson.D containing name, age, and the _oteltrace entry.
	require.GreaterOrEqual(t, len(out), 3)
	keys := make(map[string]bool)
	for _, e := range out {
		keys[e.Key] = true
	}
	assert.True(t, keys["name"])
	assert.True(t, keys["age"])
	assert.True(t, keys[TraceMetadataKey])
}

// TestInjectUpdateDoesNotMutateOriginalBsonD verifies the M1 fast path for
// update operations clones the top-level bson.D.
func TestInjectUpdateDoesNotMutateOriginalBsonD(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	orig := bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: 1}}}}

	_, err := InjectTraceIntoUpdate(ctx, orig, prop)
	require.NoError(t, err)

	// Top-level caller bson.D must still be length 1 (only $set).
	require.Len(t, orig, 1)
	assert.Equal(t, "$set", orig[0].Key)

	// $set sub-doc must not have _oteltrace appended in caller's storage.
	subDoc, ok := orig[0].Value.(bson.D)
	require.True(t, ok)
	for _, e := range subDoc {
		assert.NotEqual(t, TraceMetadataKey, e.Key, "caller $set sub-doc must not be mutated")
	}
}

// TestInjectUpdateDoesNotShareSetBacking ensures upsertSetField clones the
// $set sub-doc — without the M1 clone fix, appending to the caller's $set
// bson.D after Inject would overwrite the injected metadata entry.
func TestInjectUpdateDoesNotShareSetBacking(t *testing.T) {
	ctx, prop := ctxWithSampledSpan(t)
	setDoc := make(bson.D, 0, 10)
	setDoc = append(setDoc, bson.E{Key: "x", Value: 1})
	orig := bson.D{{Key: "$set", Value: setDoc}}

	out, err := InjectTraceIntoUpdate(ctx, orig, prop)
	require.NoError(t, err)

	outD, ok := out.(bson.D)
	require.True(t, ok)
	outSet, ok := outD[0].Value.(bson.D)
	require.True(t, ok)
	// outSet[1] should be _oteltrace.
	require.Equal(t, TraceMetadataKey, outSet[1].Key)

	// Caller appends to their $set bson.D backing array.
	_ = append(setDoc, bson.E{Key: "intruder", Value: "X"})

	// outSet[1] must remain _oteltrace untouched.
	assert.Equal(t, TraceMetadataKey, outSet[1].Key, "$set backing array must not be shared with caller")
}
