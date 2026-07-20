#include "devlogd_supervisor.hpp"

#include <arpa/inet.h>
#include <fcntl.h>
#include <netdb.h>
#include <signal.h>
#include <spawn.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cerrno>
#include <cstring>
#include <stdexcept>
#include <thread>

extern char** environ;

namespace devlog {
namespace {

// Minimal blocking HTTP/1.0 GET that returns the status code, or -1 on any
// transport error. Kept dependency-free (no libcurl) on purpose — the probe is
// a loopback call to devlogd's own health endpoint.
int http_get_status(const std::string& host, int port, const std::string& path) {
  addrinfo hints{};
  hints.ai_family = AF_UNSPEC;
  hints.ai_socktype = SOCK_STREAM;
  addrinfo* res = nullptr;
  const std::string port_s = std::to_string(port);
  if (getaddrinfo(host.c_str(), port_s.c_str(), &hints, &res) != 0) return -1;

  int status = -1;
  for (addrinfo* ai = res; ai != nullptr; ai = ai->ai_next) {
    int fd = ::socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
    if (fd < 0) continue;
    if (::connect(fd, ai->ai_addr, ai->ai_addrlen) == 0) {
      const std::string req = "GET " + path + " HTTP/1.0\r\nHost: " + host +
                              "\r\nConnection: close\r\n\r\n";
      if (::write(fd, req.data(), req.size()) == static_cast<ssize_t>(req.size())) {
        char buf[256];
        const ssize_t n = ::read(fd, buf, sizeof(buf) - 1);
        if (n > 0) {
          buf[n] = '\0';
          // Expect "HTTP/1.x <code> ...".
          int code = 0;
          if (std::sscanf(buf, "HTTP/%*d.%*d %d", &code) == 1) status = code;
        }
      }
    }
    ::close(fd);
    if (status != -1) break;
  }
  freeaddrinfo(res);
  return status;
}

}  // namespace

DevlogdSupervisor::DevlogdSupervisor(SupervisorOptions opts) : opts_(std::move(opts)) {}

DevlogdSupervisor::~DevlogdSupervisor() { stop(); }

bool DevlogdSupervisor::probe_ready() const noexcept {
  return http_get_status(opts_.health_host, opts_.health_port, opts_.health_path) == 200;
}

void DevlogdSupervisor::start() {
  if (pid_ > 0) throw std::runtime_error("devlogd already started");

  // argv: devlogd -config <path>
  std::string arg0 = opts_.binary_path;
  std::string arg1 = "-config";
  std::string arg2 = opts_.config_path;
  char* argv[] = {arg0.data(), arg1.data(), arg2.data(), nullptr};

  // Build envp = inherited environment + explicit overrides.
  std::vector<std::string> env_store;
  for (char** e = environ; *e != nullptr; ++e) env_store.emplace_back(*e);
  for (const auto& [k, v] : opts_.env) env_store.emplace_back(k + "=" + v);
  std::vector<char*> envp;
  envp.reserve(env_store.size() + 1);
  for (auto& s : env_store) envp.push_back(s.data());
  envp.push_back(nullptr);

  // Optionally redirect the child's stdout/stderr to a log file.
  posix_spawn_file_actions_t actions;
  posix_spawn_file_actions_init(&actions);
  bool have_actions = false;
  if (!opts_.log_file.empty()) {
    posix_spawn_file_actions_addopen(&actions, STDOUT_FILENO, opts_.log_file.c_str(),
                                     O_WRONLY | O_CREAT | O_APPEND, 0640);
    posix_spawn_file_actions_adddup2(&actions, STDOUT_FILENO, STDERR_FILENO);
    have_actions = true;
  }

  const int rc = posix_spawn(&pid_, opts_.binary_path.c_str(),
                             have_actions ? &actions : nullptr, nullptr, argv, envp.data());
  posix_spawn_file_actions_destroy(&actions);
  if (rc != 0) {
    pid_ = -1;
    throw std::runtime_error("posix_spawn devlogd failed: " + std::string(std::strerror(rc)));
  }

  // Poll readiness; fail fast if the child dies during startup (bad config,
  // unreachable Redis/MinIO, missing keys — devlogd exits non-zero).
  const auto deadline = std::chrono::steady_clock::now() + opts_.ready_timeout;
  while (std::chrono::steady_clock::now() < deadline) {
    if (!running()) {
      throw std::runtime_error("devlogd exited during startup (check its logs / config)");
    }
    if (probe_ready()) return;
    std::this_thread::sleep_for(std::chrono::milliseconds(200));
  }
  stop();
  throw std::runtime_error("devlogd did not become ready within the timeout");
}

bool DevlogdSupervisor::running() noexcept {
  if (pid_ <= 0) return false;
  int st = 0;
  const pid_t r = ::waitpid(pid_, &st, WNOHANG);
  if (r == 0) return true;   // still alive
  if (r == pid_) pid_ = -1;  // exited and reaped
  return false;
}

void DevlogdSupervisor::stop() noexcept {
  if (pid_ <= 0) return;

  // SIGTERM triggers devlogd's graceful shutdown: it stops accepting, drains
  // the archiver, and exits. Give it stop_timeout before escalating.
  ::kill(pid_, SIGTERM);
  const auto deadline = std::chrono::steady_clock::now() + opts_.stop_timeout;
  while (std::chrono::steady_clock::now() < deadline) {
    if (!running()) return;
    std::this_thread::sleep_for(std::chrono::milliseconds(100));
  }
  // Grace window expired — force it and reap.
  if (pid_ > 0) {
    ::kill(pid_, SIGKILL);
    int st = 0;
    ::waitpid(pid_, &st, 0);
    pid_ = -1;
  }
}

}  // namespace devlog
