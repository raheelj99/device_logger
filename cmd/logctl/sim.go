package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/config"
)

// cmdSim publishes a full sanitization job over MQTT/TLS: exactly the
// message sequence the real C++ application emits.
func cmdSim(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	addr := fs.String("mqtt", "localhost:8883", "devlogd MQTT address")
	caFile := fs.String("ca", "deploy/certs/ca.crt", "CA certificate to trust")
	licFile := fs.String("license", "", "device license file (required)")
	device := fs.String("device", "station-01", "device id (must match license subject)")
	job := fs.String("job", "", "job / trace id (default generated)")
	delay := fs.Duration("delay", 300*time.Millisecond, "delay between messages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *licFile == "" {
		return fmt.Errorf("-license is required")
	}
	token, err := os.ReadFile(*licFile)
	if err != nil {
		return fmt.Errorf("read license: %w", err)
	}
	tlsCfg, err := config.ClientTLS(*caFile)
	if err != nil {
		return err
	}
	jobID := *job
	if jobID == "" {
		jobID = "job-" + ulid.Make().String()
	}

	opts := pahomqtt.NewClientOptions().
		AddBroker("ssl://" + *addr).
		SetClientID(*device).
		SetUsername(*device).
		SetPassword(strings.TrimSpace(string(token))).
		SetTLSConfig(tlsCfg).
		SetConnectTimeout(10 * time.Second)
	client := pahomqtt.NewClient(opts)
	if t := client.Connect(); t.Wait() && t.Error() != nil {
		return fmt.Errorf("mqtt connect: %w", t.Error())
	}
	defer client.Disconnect(250)
	fmt.Printf("connected as %s, running job %s\n", *device, jobID)

	var seq uint64
	publish := func(subsystem string, e *devicelogv1.LogEntry) error {
		seq++
		e.DeviceId = *device
		e.Seq = seq
		e.DeviceTime = timestamppb.Now()
		e.TraceId = jobID
		payload, err := proto.Marshal(e)
		if err != nil {
			return err
		}
		topic := fmt.Sprintf("devlog/v1/%s/%s", *device, subsystem)
		if t := client.Publish(topic, 1, false, payload); t.Wait() && t.Error() != nil {
			return fmt.Errorf("publish: %w", t.Error())
		}
		time.Sleep(*delay)
		return nil
	}

	media := &devicelogv1.TargetMedia{
		Serial:        "WD-9F2K3L0042",
		Model:         "WDC WD40EFRX",
		CapacityBytes: 4 << 40,
		MediaType:     "HDD",
	}
	event := func(phase devicelogv1.SanitizationPhase, progress float64) *devicelogv1.SanitizationEvent {
		return &devicelogv1.SanitizationEvent{
			Media:       media,
			Standard:    devicelogv1.SanitizationStandard_SANITIZATION_STANDARD_NIST_800_88_PURGE,
			Technique:   "overwrite-1pass",
			Phase:       phase,
			ProgressPct: progress,
			OperatorId:  "op-raheel",
		}
	}

	steps := []struct {
		severity  devicelogv1.Severity
		subsystem string
		message   string
		san       *devicelogv1.SanitizationEvent
	}{
		{devicelogv1.Severity_SEVERITY_INFO, "system", "station self-check passed", nil},
		{devicelogv1.Severity_SEVERITY_INFO, "sanitizer", "sanitization started",
			event(devicelogv1.SanitizationPhase_SANITIZATION_PHASE_STARTED, 0)},
		{devicelogv1.Severity_SEVERITY_INFO, "sanitizer", "overwrite pass in progress",
			event(devicelogv1.SanitizationPhase_SANITIZATION_PHASE_PROGRESS, 25)},
		{devicelogv1.Severity_SEVERITY_WARN, "smart", "reallocated sector count above baseline", nil},
		{devicelogv1.Severity_SEVERITY_INFO, "sanitizer", "overwrite pass in progress",
			event(devicelogv1.SanitizationPhase_SANITIZATION_PHASE_PROGRESS, 75)},
		{devicelogv1.Severity_SEVERITY_INFO, "sanitizer", "verifying sampled sectors",
			event(devicelogv1.SanitizationPhase_SANITIZATION_PHASE_VERIFYING, 100)},
	}
	for _, s := range steps {
		e := &devicelogv1.LogEntry{
			Severity: s.severity, Subsystem: s.subsystem, Message: s.message, Sanitization: s.san,
			Attributes: map[string]string{"firmware": "2.4.1"},
		}
		if err := publish(s.subsystem, e); err != nil {
			return err
		}
		fmt.Println("→", s.message)
	}

	done := event(devicelogv1.SanitizationPhase_SANITIZATION_PHASE_COMPLETED, 100)
	done.Verification = &devicelogv1.Verification{Method: "sampled-read", SamplePct: 10, Passed: true}
	if err := publish("sanitizer", &devicelogv1.LogEntry{
		Severity:  devicelogv1.Severity_SEVERITY_INFO,
		Subsystem: "sanitizer",
		Message:   "sanitization completed, verification passed",
		Sanitization: done,
	}); err != nil {
		return err
	}
	fmt.Println("→ sanitization completed, verification passed")
	fmt.Printf("\njob done. try:\n  logctl query -trace %s -license operator.lic\n  logctl export -trace %s -license operator.lic\n", jobID, jobID)
	return nil
}
