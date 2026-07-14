// OTel database and MongoDB semantic convention helpers.

package shared

import (
	"errors"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	keyDBSystemName         = "db.system.name"
	keyDBCollection         = "db.collection.name"
	keyDBNamespace          = "db.namespace"
	keyDBOperationName      = "db.operation.name"
	keyDBOpBatchSize        = "db.operation.batch.size"
	keyDBResponseStatusCode = "db.response.status_code"
	keyErrorType            = "error.type"
	keyServerAddress        = "server.address"
	keyServerPort           = "server.port"
)

const (
	dbSystemMongoDB = "mongodb"
	errorTypeOther  = "_OTHER"
)

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

// RecordSpanError sets span status to Error and records db.response.status_code and error.type.
func RecordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())

	var writeErr mongo.WriteException
	if errors.As(err, &writeErr) {
		errCodes := writeErr.ErrorCodes()
		if len(errCodes) > 0 {
			codeStr := strconv.Itoa(errCodes[0])
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
