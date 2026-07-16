// Reference C++ producer for devlogd: publishes one sanitization job over
// MQTT/TLS, exactly like cmd/logctl's `sim` command does in Go.
//
// The contract with devlogd:
//   - broker:    ssl://<host>:8883, TLS with the deployment's CA
//   - username:  the device id (must match the license subject)
//   - password:  the signed license token (content of the .lic file)
//   - topic:     devlog/v1/<device_id>/<subsystem>
//   - payload:   serialized devicelog.v1.LogEntry (leave entry_id,
//                ingest_time and audit empty — the server owns them)

#include <chrono>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>
#include <thread>

#include <mqtt/async_client.h>

#include "devicelog/v1/log.pb.h"

namespace dl = devicelog::v1;

static std::string read_file(const std::string& path) {
  std::ifstream f(path, std::ios::binary);
  if (!f) throw std::runtime_error("cannot read " + path);
  std::ostringstream ss;
  ss << f.rdbuf();
  return ss.str();
}

static void set_now(google::protobuf::Timestamp* ts) {
  const auto now = std::chrono::system_clock::now().time_since_epoch();
  ts->set_seconds(std::chrono::duration_cast<std::chrono::seconds>(now).count());
  ts->set_nanos(static_cast<int32_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(now).count() % 1000000000));
}

class JobLogger {
 public:
  JobLogger(mqtt::async_client& client, std::string device, std::string job_id)
      : client_(client), device_(std::move(device)), job_id_(std::move(job_id)) {}

  void publish(dl::Severity severity, const std::string& subsystem,
               const std::string& message, const dl::SanitizationEvent* san = nullptr) {
    dl::LogEntry e;
    e.set_device_id(device_);
    e.set_seq(++seq_);
    e.set_severity(severity);
    e.set_subsystem(subsystem);
    e.set_message(message);
    e.set_trace_id(job_id_);
    set_now(e.mutable_device_time());
    if (san) *e.mutable_sanitization() = *san;

    std::string payload;
    e.SerializeToString(&payload);
    const std::string topic = "devlog/v1/" + device_ + "/" + subsystem;
    client_.publish(topic, payload.data(), payload.size(), /*qos=*/1, /*retained=*/false)->wait();
    std::cout << "-> " << message << "\n";
    std::this_thread::sleep_for(std::chrono::milliseconds(300));
  }

 private:
  mqtt::async_client& client_;
  std::string device_;
  std::string job_id_;
  uint64_t seq_ = 0;
};

int main(int argc, char** argv) {
  const std::string host = argc > 1 ? argv[1] : "localhost:8883";
  const std::string device = argc > 2 ? argv[2] : "station-01";
  const std::string lic_path = argc > 3 ? argv[3] : "station-01.lic";
  const std::string ca_path = argc > 4 ? argv[4] : "deploy/certs/ca.crt";

  try {
    const std::string license_token = read_file(lic_path);

    mqtt::async_client client("ssl://" + host, device);
    auto ssl = mqtt::ssl_options_builder().trust_store(ca_path).finalize();
    auto conn = mqtt::connect_options_builder()
                    .user_name(device)
                    .password(license_token)
                    .ssl(ssl)
                    .finalize();
    client.connect(conn)->wait();
    std::cout << "connected as " << device << "\n";

    const std::string job_id = "job-cpp-" + std::to_string(std::time(nullptr));
    JobLogger log(client, device, job_id);

    dl::SanitizationEvent san;
    auto* media = san.mutable_media();
    media->set_serial("WD-9F2K3L0042");
    media->set_model("WDC WD40EFRX");
    media->set_capacity_bytes(4ULL << 40);
    media->set_media_type("HDD");
    san.set_standard(dl::SANITIZATION_STANDARD_NIST_800_88_PURGE);
    san.set_technique("overwrite-1pass");
    san.set_operator_id("op-raheel");

    san.set_phase(dl::SANITIZATION_PHASE_STARTED);
    log.publish(dl::SEVERITY_INFO, "sanitizer", "sanitization started", &san);

    for (double pct : {25.0, 50.0, 75.0}) {
      san.set_phase(dl::SANITIZATION_PHASE_PROGRESS);
      san.set_progress_pct(pct);
      log.publish(dl::SEVERITY_INFO, "sanitizer", "overwrite pass in progress", &san);
    }

    san.set_phase(dl::SANITIZATION_PHASE_VERIFYING);
    san.set_progress_pct(100);
    log.publish(dl::SEVERITY_INFO, "sanitizer", "verifying sampled sectors", &san);

    san.set_phase(dl::SANITIZATION_PHASE_COMPLETED);
    auto* verification = san.mutable_verification();
    verification->set_method("sampled-read");
    verification->set_sample_pct(10);
    verification->set_passed(true);
    log.publish(dl::SEVERITY_INFO, "sanitizer",
                "sanitization completed, verification passed", &san);

    client.disconnect()->wait();
    std::cout << "job " << job_id << " published\n";
    return 0;
  } catch (const std::exception& e) {
    std::cerr << "publisher: " << e.what() << "\n";
    return 1;
  }
}
