package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/config"
)

// connFlags are shared by every gRPC subcommand.
type connFlags struct {
	addr    *string
	caFile  *string
	licFile *string
}

func addConnFlags(fs *flag.FlagSet) *connFlags {
	return &connFlags{
		addr:    fs.String("grpc", "localhost:9443", "devlogd gRPC address"),
		caFile:  fs.String("ca", "deploy/certs/ca.crt", "CA certificate to trust"),
		licFile: fs.String("license", "operator.lic", "license file presented as bearer token"),
	}
}

// bearerToken sends the license as per-RPC metadata; it refuses to run
// without transport security so the token never travels in the clear.
type bearerToken string

func (b bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearerToken) RequireTransportSecurity() bool { return true }

func dial(c *connFlags) (*grpc.ClientConn, devicelogv1.LogServiceClient, error) {
	tlsCfg, err := config.ClientTLS(*c.caFile)
	if err != nil {
		return nil, nil, err
	}
	token, err := os.ReadFile(*c.licFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read license: %w", err)
	}
	conn, err := grpc.NewClient(*c.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(bearerToken(strings.TrimSpace(string(token)))),
	)
	if err != nil {
		return nil, nil, err
	}
	return conn, devicelogv1.NewLogServiceClient(conn), nil
}

func severityFromString(s string) (devicelogv1.Severity, error) {
	switch strings.ToLower(s) {
	case "":
		return devicelogv1.Severity_SEVERITY_UNSPECIFIED, nil
	case "trace":
		return devicelogv1.Severity_SEVERITY_TRACE, nil
	case "debug":
		return devicelogv1.Severity_SEVERITY_DEBUG, nil
	case "info":
		return devicelogv1.Severity_SEVERITY_INFO, nil
	case "warn":
		return devicelogv1.Severity_SEVERITY_WARN, nil
	case "error":
		return devicelogv1.Severity_SEVERITY_ERROR, nil
	case "fatal":
		return devicelogv1.Severity_SEVERITY_FATAL, nil
	default:
		return 0, fmt.Errorf("unknown severity %q", s)
	}
}

func printEntry(e *devicelogv1.LogEntry) {
	b, err := protojson.Marshal(e)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logctl: marshal:", err)
		return
	}
	fmt.Println(string(b))
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	c := addConnFlags(fs)
	device := fs.String("device", "", "device id (empty = all)")
	since := fs.Duration("since", time.Hour, "lookback window")
	severity := fs.String("severity", "", "minimum severity (trace..fatal)")
	subsystem := fs.String("subsystem", "", "subsystem filter")
	trace := fs.String("trace", "", "trace / job id filter")
	limit := fs.Uint("limit", 1000, "maximum entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sev, err := severityFromString(*severity)
	if err != nil {
		return err
	}
	conn, client, err := dial(c)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &devicelogv1.QueryRequest{
		From:        timestamppb.New(time.Now().Add(-*since)),
		MinSeverity: sev,
		Subsystem:   *subsystem,
		TraceId:     *trace,
		Limit:       uint32(*limit),
	}
	if *device != "" {
		req.DeviceIds = []string{*device}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stream, err := client.Query(ctx, req)
	if err != nil {
		return err
	}
	n := 0
	for {
		e, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		printEntry(e)
		n++
	}
	fmt.Fprintf(os.Stderr, "%d entries\n", n)
	return nil
}

func cmdTail(args []string) error {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	c := addConnFlags(fs)
	device := fs.String("device", "", "device id (empty = all)")
	severity := fs.String("severity", "", "minimum severity")
	trace := fs.String("trace", "", "trace / job id filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sev, err := severityFromString(*severity)
	if err != nil {
		return err
	}
	conn, client, err := dial(c)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	req := &devicelogv1.TailRequest{MinSeverity: sev, TraceId: *trace}
	if *device != "" {
		req.DeviceIds = []string{*device}
	}
	stream, err := client.Tail(ctx, req)
	if err != nil {
		return err
	}
	for {
		e, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil // interrupted by user
			}
			return err
		}
		printEntry(e)
	}
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	c := addConnFlags(fs)
	device := fs.String("device", "", "device id (required)")
	since := fs.Duration("since", 24*time.Hour, "lookback window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *device == "" {
		return fmt.Errorf("-device is required")
	}
	conn, client, err := dial(c)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resp, err := client.VerifyRange(ctx, &devicelogv1.VerifyRangeRequest{
		DeviceId: *device,
		From:     timestamppb.New(time.Now().Add(-*since)),
	})
	if err != nil {
		return err
	}
	if resp.Ok {
		fmt.Printf("OK: %d entries verified, chain intact\n", resp.EntriesChecked)
		return nil
	}
	fmt.Printf("TAMPER EVIDENCE: %d entries checked, %d problems\n", resp.EntriesChecked, len(resp.Breaks))
	for _, b := range resp.Breaks {
		fmt.Printf("  entry %s: %s\n", b.EntryId, b.Reason)
	}
	return fmt.Errorf("verification failed")
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	c := addConnFlags(fs)
	trace := fs.String("trace", "", "sanitization job / trace id (required)")
	since := fs.Duration("since", 30*24*time.Hour, "lookback window")
	out := fs.String("out", "", "output file (default <trace>.report.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *trace == "" {
		return fmt.Errorf("-trace is required")
	}
	conn, client, err := dial(c)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := client.ExportAuditReport(ctx, &devicelogv1.ExportAuditReportRequest{
		TraceId: *trace,
		From:    timestamppb.New(time.Now().Add(-*since)),
	})
	if err != nil {
		return err
	}
	b, err := protojson.MarshalOptions{Multiline: true}.Marshal(rep)
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		path = *trace + ".report.json"
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	verdict := "ALL SIGNATURES VALID"
	if !rep.AllSignaturesValid {
		verdict = "SIGNATURE PROBLEMS DETECTED"
	}
	fmt.Printf("report for %s: %d entries, %d signatures verified — %s → %s\n",
		rep.TraceId, len(rep.Entries), rep.SignaturesVerified, verdict, path)
	return nil
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	c := addConnFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, client, err := dial(c)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.GetStats(ctx, &devicelogv1.GetStatsRequest{})
	if err != nil {
		return err
	}
	for _, d := range resp.Devices {
		last := "never"
		if d.LastIngest != nil {
			last = d.LastIngest.AsTime().Local().Format(time.RFC3339)
		}
		fmt.Printf("%-24s hot_entries=%-8d last_ingest=%s\n", d.DeviceId, d.HotEntries, last)
	}
	return nil
}
