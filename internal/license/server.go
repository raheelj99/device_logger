package license

import (
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Server is the mini license server (cmd/licensed): it re-verifies presented
// licenses and enforces max_sessions across devices.
type Server struct {
	ver *Verifier
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]map[string]struct{} // license ID → device ids
}

func NewServer(issuerPub ed25519.PublicKey, log *slog.Logger) *Server {
	return &Server{
		ver:      NewVerifier(issuerPub),
		log:      log,
		sessions: map[string]map[string]struct{}{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/activate", s.activate)
	mux.HandleFunc("POST /v1/heartbeat", s.activate) // idempotent re-activation
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) activate(w http.ResponseWriter, r *http.Request) {
	var req ActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, http.StatusBadRequest, ActivateResponse{Reason: "malformed request"})
		return
	}
	signed, err := Parse(req.Token)
	if err != nil {
		respond(w, http.StatusForbidden, ActivateResponse{Reason: err.Error()})
		return
	}
	if err := s.ver.Verify(signed, req.DeviceID, "", time.Now()); err != nil {
		s.log.Warn("activation rejected", "device", req.DeviceID, "err", err)
		respond(w, http.StatusForbidden, ActivateResponse{Reason: err.Error()})
		return
	}

	lic := signed.License
	s.mu.Lock()
	devices, ok := s.sessions[lic.ID]
	if !ok {
		devices = map[string]struct{}{}
		s.sessions[lic.ID] = devices
	}
	_, known := devices[req.DeviceID]
	if !known && lic.MaxSessions > 0 && len(devices) >= lic.MaxSessions {
		s.mu.Unlock()
		s.log.Warn("activation rejected: session limit", "license", lic.ID, "device", req.DeviceID)
		respond(w, http.StatusForbidden, ActivateResponse{Reason: "max sessions exceeded"})
		return
	}
	devices[req.DeviceID] = struct{}{}
	s.mu.Unlock()

	s.log.Info("session activated", "license", lic.ID, "device", req.DeviceID)
	respond(w, http.StatusOK, ActivateResponse{OK: true, ExpiresAt: lic.NotAfter})
}

func respond(w http.ResponseWriter, status int, body ActivateResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
