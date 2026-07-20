// Example: your C++ application starts devlogd as it initializes, then shuts it
// down cleanly on exit. This is the whole integration — the rest of your app
// talks to devlogd over MQTT (see examples/cpp/publisher) exactly as before.
#include <csignal>
#include <cstdlib>
#include <iostream>

#include "devlogd_supervisor.hpp"

namespace {
volatile std::sig_atomic_t g_stop = 0;
void on_signal(int) { g_stop = 1; }
}  // namespace

int main() {
  std::signal(SIGINT, on_signal);
  std::signal(SIGTERM, on_signal);

  devlog::SupervisorOptions opts;
  // Point these at where your .deb installs devlogd and its config.
  opts.binary_path = std::getenv("DEVLOGD_BIN") ? std::getenv("DEVLOGD_BIN") : "/opt/devlog/bin/devlogd";
  opts.config_path = std::getenv("DEVLOGD_CFG") ? std::getenv("DEVLOGD_CFG") : "/etc/devlog/devlogd.yaml";
  opts.log_file = "/var/log/devlog/devlogd.log";

  devlog::DevlogdSupervisor devlogd(opts);
  try {
    std::cout << "starting devlogd…\n";
    devlogd.start();  // blocks until /readyz == 200 (or throws)
    std::cout << "devlogd ready (pid " << devlogd.pid() << ")\n";
  } catch (const std::exception& e) {
    // Fail-fast policy: if audit logging can't start, neither should the app
    // that depends on it for compliance. Adjust if logging is best-effort.
    std::cerr << "FATAL: could not start devlogd: " << e.what() << "\n";
    return 1;
  }

  // --- your application runs here ---
  while (!g_stop) {
    if (!devlogd.running()) {
      std::cerr << "devlogd died unexpectedly; aborting\n";
      return 1;
    }
    ::pause();  // replace with your real event loop
  }

  std::cout << "shutting down; draining devlogd…\n";
  devlogd.stop();  // graceful: SIGTERM → archiver flush → exit
  std::cout << "done\n";
  return 0;
}
