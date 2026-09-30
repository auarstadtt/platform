package platform

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// MetadataRequestID is the gRPC metadata key carrying the correlation ID.
// Lower-case because gRPC normalises metadata keys to lower-case; writing it
// mixed-case would produce a key that never matches on read.
const MetadataRequestID = "x-request-id"

// requestIDFromMD adopts a valid inbound correlation ID from metadata, or mints
// one. Shares validInboundID with the HTTP path so an ID crossing HTTP -> gRPC
// is subject to the same restrictions and cannot inject into logs at the second
// hop.
func requestIDFromMD(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals := md.Get(MetadataRequestID); len(vals) > 0 && validInboundID(vals[0]) {
			return vals[0]
		}
	}
	return NewRequestID()
}

// UnaryServerInterceptor gives gRPC services the same correlation and
// request-scoped logging as the HTTP chain: it adopts or mints a request ID,
// attaches a logger carrying it, and makes both available via RequestIDFrom(ctx)
// and Logger(ctx).
//
// Exists because some services serve only gRPC. An HTTP-only chain would leave
// them with exactly the unattributable logging this module was built to
// eliminate.
//
// Rate limiting is intentionally absent here: gRPC limiting keys on peer identity
// rather than IP and belongs with the service's auth interceptor, which this
// module does not own.
func UnaryServerInterceptor(logger *zap.Logger, serviceName string) grpc.UnaryServerInterceptor {
	base := logger
	if base == nil {
		base = fallbackLogger
	}
	if serviceName != "" {
		base = base.With(zap.String("service", serviceName))
	}
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		id := requestIDFromMD(ctx)
		l := base.With(
			zap.String("request_id", id),
			zap.String("grpc_method", info.FullMethod),
		)
		ctx = WithLogger(WithRequestID(ctx, id), l)
		return handler(ctx, req)
	}
}

// OutgoingRequestID propagates the context's correlation ID onto an outbound gRPC
// call, so a request traversing several services shares one ID end to end.
//
// Without this the chain breaks at every hop and per-service logs cannot be
// stitched together — which is most of the value of having correlation at all.
func OutgoingRequestID(ctx context.Context) context.Context {
	id := RequestIDFrom(ctx)
	if id == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, MetadataRequestID, id)
}
