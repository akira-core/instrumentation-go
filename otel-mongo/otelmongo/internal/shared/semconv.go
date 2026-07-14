// OTel database and MongoDB semantic convention helpers.
// See https://opentelemetry.io/docs/specs/semconv/db/database-spans/ and
// https://opentelemetry.io/docs/specs/semconv/db/mongodb/

package shared

import (
	"errors"
	"strconv"

	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	keyDBSystemName          = "db.system.name"
	keyDBCollection          = "db.collection.name"
	keyDBNamespace           = "db.namespace"
	keyDBOperationName       = "db.operation.name"
	keyDBOpBatchSize         = "db.operation.batch.size"
	keyDBResponseStatusCode  = "db.response.status_code"
	keyErrorType             = "error.type"
	keyServerAddress         = "server.address"
	keyServerPort            = "server.port"
	keyServerAddressFallback = "mongodb.server_address.fallback"
)

const (
	dbSystemMongoDB = "mongodb"
	errorTypeOther  = "_OTHER"
)

// DeliverAttributes returns the attribute set for a MongoDB deliver span
// (the synthetic CONSUMER span that represents broker delivery). Caller passes
// only the values it has — semconv key + conditional defaulting stays here.
func DeliverAttributes(dbName, collName, serverAddr string, serverPort int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(keyDBSystemName, dbSystemMongoDB),
		attribute.String(keyDBCollection, collName),
	}
	if dbName != "" {
		attrs = append(attrs, attribute.String(keyDBNamespace, dbName))
	}
	if serverAddr != "" {
		attrs = append(attrs, attribute.String(keyServerAddress, serverAddr))
		if serverPort > 0 && serverPort != 27017 {
			attrs = append(attrs, attribute.Int(keyServerPort, serverPort))
		}
	}
	return attrs
}

// DBSpanName returns the span name per OTel: "{db.operation.name} {target}".
func DBSpanName(operation, collectionName string) string {
	if collectionName == "" {
		return operation
	}
	return operation + " " + collectionName
}

// DBAttributes returns attributes for a MongoDB client span. It emits db.* only;
// server.address/server.port are emitted once, post-call, via ServerAttributes
// (see monitor.go) from the per-command captured address, so the exported
// server.* is always a same-source pair with no stale-port hazard.
func DBAttributes(dbName, collName, operation string, batchSize int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(keyDBSystemName, dbSystemMongoDB),
		attribute.String(keyDBCollection, collName),
		attribute.String(keyDBOperationName, operation),
	}
	if dbName != "" {
		attrs = append(attrs, attribute.String(keyDBNamespace, dbName))
	}
	if batchSize >= 2 {
		attrs = append(attrs, attribute.Int(keyDBOpBatchSize, batchSize))
	}
	return attrs
}

// ServerAttributes returns the server.address/server.port attribute pair for
// serverAddr/serverPort, following semconv defaulting rules: nil when
// serverAddr is empty, server.port omitted when serverPort is the MongoDB
// default (27017). It is the sole emitter of server.*, called once post-call
// with the per-command captured address (see monitor.go); DBAttributes no
// longer emits server.* at span start.
func ServerAttributes(serverAddr string, serverPort int) []attribute.KeyValue {
	if serverAddr == "" {
		return nil
	}
	attrs := []attribute.KeyValue{attribute.String(keyServerAddress, serverAddr)}
	if serverPort > 0 && serverPort != 27017 {
		attrs = append(attrs, attribute.Int(keyServerPort, serverPort))
	}
	return attrs
}

// ServerAddressFallbackAttribute marks a span whose server.* came from the
// static Connect-time URI parse rather than a per-command CommandMonitor
// capture (see monitor.go). Temporary debug aid for telling the two sources
// apart in production traces.
func ServerAddressFallbackAttribute() attribute.KeyValue {
	return attribute.Bool(keyServerAddressFallback, true)
}

// RecordSpanError sets span status to Error and records db.response.status_code and error.type.
func RecordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())

	var writeErr mongo.WriteException
	if errors.As(err, &writeErr) {
		writeErrors := writeErr.WriteErrors
		if len(writeErrors) > 0 {
			codeStr := strconv.Itoa(writeErrors[0].Code)
			span.SetAttributes(
				attribute.String(keyDBResponseStatusCode, codeStr),
				attribute.String(keyErrorType, codeStr),
			)
			return
		}
	}

	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		codeStr := strconv.Itoa(int(cmdErr.Code))
		span.SetAttributes(
			attribute.String(keyDBResponseStatusCode, codeStr),
			attribute.String(keyErrorType, codeStr),
		)
		return
	}

	span.SetAttributes(attribute.String(keyErrorType, errorTypeOther))
}
