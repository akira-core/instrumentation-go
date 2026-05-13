package direct

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// SingleResult is the disabled-path passthrough impl.
type SingleResult struct {
	sr  *mongo.SingleResult
	ctx context.Context
}

// NewSingleResult wraps sr with the disabled-path passthrough SingleResult.
func NewSingleResult(sr *mongo.SingleResult, ctx context.Context) *SingleResult {
	return &SingleResult{sr: sr, ctx: ctx}
}

// Decode delegates to *mongo.SingleResult.Decode.
func (r *SingleResult) Decode(v any) error { return r.sr.Decode(v) }

// TraceContext returns the parent context unchanged.
func (r *SingleResult) TraceContext() context.Context { return r.ctx }

// Raw delegates to *mongo.SingleResult.Raw.
func (r *SingleResult) Raw() (bson.Raw, error) { return r.sr.Raw() }
