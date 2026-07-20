// DevlogdSupervisor launches and supervises the devlogd logging service as a
// child of your C++ application, so a single process controls both lifecycles.
//
// Why spawn instead of a separate systemd unit: this deployment ships devlogd
// inside the C++ app's .deb and wants the logger to live and die with the app
// on hosts that may not run systemd (field robots, containers, embedded).
//
// Contract:
//   start() spawns devlogd and blocks until it reports ready (/readyz == 200)
//   or ready_timeout elapses (throws). stop() asks it to drain gracefully
//   (SIGTERM → wait → SIGKILL). The destructor calls stop(). No dependencies
//   beyond POSIX + the C++ standard library.
#pragma once

#include <sys/types.h>

#include <chrono>
#include <string>
#include <utility>
#include <vector>

namespace devlog {

struct SupervisorOptions {
  // Absolute path to the devlogd binary shipped in your package.
  std::string binary_path = "/opt/devlog/bin/devlogd";
  // Absolute path to the YAML config (see config/devlogd.example.yaml).
  std::string config_path = "/etc/devlog/devlogd.yaml";

  // Readiness probe — must match the `http.listen` port in the config.
  std::string health_host = "127.0.0.1";
  int health_port = 9090;
  std::string health_path = "/readyz";

  // How long start() waits for readiness before giving up.
  std::chrono::milliseconds ready_timeout{30000};
  // Grace window for the archiver to flush on stop() before SIGKILL.
  std::chrono::milliseconds stop_timeout{15000};

  // Extra environment for the child (e.g. {"DEVLOG_LOG_LEVEL","debug"}).
  // The child otherwise inherits the parent's environment.
  std::vector<std::pair<std::string, std::string>> env;

  // Redirect devlogd's stdout/stderr to this file (append). Empty = inherit
  // the parent's streams (e.g. journald when the C++ app is a systemd unit).
  std::string log_file;
};

class DevlogdSupervisor {
 public:
  explicit DevlogdSupervisor(SupervisorOptions opts);
  ~DevlogdSupervisor();  // calls stop()

  DevlogdSupervisor(const DevlogdSupervisor&) = delete;
  DevlogdSupervisor& operator=(const DevlogdSupervisor&) = delete;

  // Spawn devlogd, then poll /readyz until 200 or ready_timeout.
  // Throws std::runtime_error on spawn failure, early child exit, or timeout.
  void start();

  // Graceful shutdown: SIGTERM, wait up to stop_timeout, then SIGKILL.
  // Idempotent and safe to call from a signal-handling path.
  void stop() noexcept;

  // Non-blocking liveness check; reaps the child if it has exited.
  bool running() noexcept;

  pid_t pid() const noexcept { return pid_; }

 private:
  bool probe_ready() const noexcept;  // one /readyz HTTP GET, true on 200

  SupervisorOptions opts_;
  pid_t pid_ = -1;
};

}  // namespace devlog
