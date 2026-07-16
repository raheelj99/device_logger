package license

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"devlog/internal/config"
)

type ActivateRequest struct {
	Token    string `json:"token"`
	DeviceID string `json:"device_id"`
}

type ActivateResponse struct {
	OK        bool      `json:"ok"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// OnlineValidator activates sessions against the license server. If the
// server is unreachable, sessions that activated successfully before are
// allowed for a grace period — a robot in the field must not lose logging
// because the license server is down. A definitive rejection (HTTP 403)
// always denies.
type OnlineValidator struct {
	url   string
	hc    *http.Client
	grace time.Duration
	log   *slog.Logger

	mu     sync.Mutex
	lastOK map[string]time.Time // license ID → last successful activation
}

func NewOnlineValidator(url, caFile string, grace time.Duration, log *slog.Logger) (*OnlineValidator, error) {
	tlsCfg, err := config.ClientTLS(caFile)
	if err != nil {
		return nil, err
	}
	return &OnlineValidator{
		url:    url,
		hc:     &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		grace:  grace,
		log:    log,
		lastOK: map[string]time.Time{},
	}, nil
}

func (o *OnlineValidator) Validate(ctx context.Context, s *Signed, deviceID string) error {
	token, err := s.Token()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(ActivateRequest{Token: token, DeviceID: deviceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/v1/activate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.hc.Do(req)
	if err != nil {
		return o.graceOrDeny(s.License.ID, err)
	}
	defer resp.Body.Close()

	var ar ActivateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return o.graceOrDeny(s.License.ID, fmt.Errorf("bad license server response: %w", err))
	}
	if !ar.OK {
		return fmt.Errorf("license server rejected session: %s", ar.Reason)
	}
	o.mu.Lock()
	o.lastOK[s.License.ID] = time.Now()
	o.mu.Unlock()
	return nil
}

// graceOrDeny allows a previously activated license through while the server
// is unreachable and the grace window has not lapsed.
func (o *OnlineValidator) graceOrDeny(licID string, cause error) error {
	o.mu.Lock()
	last, seen := o.lastOK[licID]
	o.mu.Unlock()
	if seen && time.Since(last) <= o.grace {
		o.log.Warn("license server unreachable, session allowed under grace",
			"license", licID, "last_activation", last, "err", cause)
		return nil
	}
	return fmt.Errorf("license server unreachable and no grace available: %w", cause)
}
