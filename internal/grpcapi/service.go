// Package grpcapi exposes the observation plane over TLS gRPC with
// license-checked, panic-safe, metered interceptors.
package grpcapi

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/hot"
	"devlog/internal/ingest"
	"devlog/internal/query"
)

type Service struct {
	devicelogv1.UnimplementedLogServiceServer
	engine *query.Engine
	hub    *ingest.Hub
	hot    *hot.Store
	log    *slog.Logger
}

func NewService(engine *query.Engine, hub *ingest.Hub, h *hot.Store, log *slog.Logger) *Service {
	return &Service{engine: engine, hub: hub, hot: h, log: log}
}

func tsOrZero(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func (s *Service) Query(req *devicelogv1.QueryRequest, stream devicelogv1.LogService_QueryServer) error {
	entries, err := s.engine.Query(stream.Context(), query.Filter{
		Devices:     req.DeviceIds,
		From:        tsOrZero(req.From),
		To:          tsOrZero(req.To),
		MinSeverity: req.MinSeverity,
		Subsystem:   req.Subsystem,
		TraceID:     req.TraceId,
		Limit:       int(req.Limit),
	})
	if err != nil {
		return status.Errorf(codes.Internal, "query: %v", err)
	}
	for _, e := range entries {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Tail(req *devicelogv1.TailRequest, stream devicelogv1.LogService_TailServer) error {
	id, ch := s.hub.Subscribe(256)
	defer s.hub.Unsubscribe(id)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if !tailMatch(e, req) {
				continue
			}
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}
}

func tailMatch(e *devicelogv1.LogEntry, req *devicelogv1.TailRequest) bool {
	if req.MinSeverity != devicelogv1.Severity_SEVERITY_UNSPECIFIED && e.Severity < req.MinSeverity {
		return false
	}
	if req.TraceId != "" && e.TraceId != req.TraceId {
		return false
	}
	if len(req.DeviceIds) == 0 {
		return true
	}
	for _, d := range req.DeviceIds {
		if e.DeviceId == d {
			return true
		}
	}
	return false
}

func (s *Service) VerifyRange(ctx context.Context, req *devicelogv1.VerifyRangeRequest) (*devicelogv1.VerifyRangeResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	resp, err := s.engine.VerifyRange(ctx, req.DeviceId, tsOrZero(req.From), tsOrZero(req.To))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "verify: %v", err)
	}
	return resp, nil
}

func (s *Service) ExportAuditReport(ctx context.Context, req *devicelogv1.ExportAuditReportRequest) (*devicelogv1.AuditReport, error) {
	if req.TraceId == "" {
		return nil, status.Error(codes.InvalidArgument, "trace_id is required")
	}
	rep, err := s.engine.AuditReport(ctx, req.TraceId, tsOrZero(req.From), tsOrZero(req.To))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit report: %v", err)
	}
	if len(rep.Entries) == 0 {
		return nil, status.Errorf(codes.NotFound, "no entries for trace %q in the requested window", req.TraceId)
	}
	return rep, nil
}

func (s *Service) GetStats(ctx context.Context, _ *devicelogv1.GetStatsRequest) (*devicelogv1.GetStatsResponse, error) {
	stats, err := s.hot.Stats(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stats: %v", err)
	}
	resp := &devicelogv1.GetStatsResponse{}
	for _, st := range stats {
		ds := &devicelogv1.DeviceStats{DeviceId: st.Device, HotEntries: uint64(st.HotEntries)}
		if !st.LastIngest.IsZero() {
			ds.LastIngest = timestamppb.New(st.LastIngest)
		}
		resp.Devices = append(resp.Devices, ds)
	}
	return resp, nil
}
