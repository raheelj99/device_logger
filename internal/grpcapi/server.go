package grpcapi

import (
	"context"
	"crypto/tls"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/license"
	"devlog/internal/telemetry"
)

// NewServer wires TLS credentials and the interceptor chain
// (recover → authorize → meter) around the LogService.
func NewServer(tlsCfg *tls.Config, sessions *license.Manager, svc *Service,
	metrics *telemetry.Metrics, log *slog.Logger) *grpc.Server {
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(unaryRecover(log), unaryAuth(sessions), unaryMetrics(metrics)),
		grpc.ChainStreamInterceptor(streamRecover(log), streamAuth(sessions), streamMetrics(metrics)),
	)
	devicelogv1.RegisterLogServiceServer(gs, svc)
	return gs
}

func authorize(ctx context.Context, sessions *license.Manager) error {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	token := strings.TrimPrefix(vals[0], "Bearer ")
	if _, err := sessions.Authorize(ctx, token, "", license.FeatureQuery); err != nil {
		return status.Errorf(codes.Unauthenticated, "license rejected: %v", err)
	}
	return nil
}

func unaryAuth(sessions *license.Manager) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(ctx, sessions); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func streamAuth(sessions *license.Manager) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(ss.Context(), sessions); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func unaryRecover(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in unary handler", "method", info.FullMethod, "panic", r)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

func streamRecover(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in stream handler", "method", info.FullMethod, "panic", r)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}

func unaryMetrics(m *telemetry.Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		m.GRPCRequests.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
		return resp, err
	}
}

func streamMetrics(m *telemetry.Metrics) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, ss)
		m.GRPCRequests.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
		return err
	}
}
