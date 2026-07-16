# C++ example publisher

Publishes a complete sanitization job to devlogd over MQTT/TLS — the same
sequence as `logctl sim`, and the template for integrating the real
sanitization application.

## Build (vcpkg)

```powershell
vcpkg install paho-mqttpp3 protobuf
cmake -B build -DCMAKE_TOOLCHAIN_FILE=$env:VCPKG_ROOT/scripts/buildsystems/vcpkg.cmake
cmake --build build --config Release
```

## Run

Arguments: `publisher [host:port] [device_id] [license.lic] [ca.crt]`

```powershell
.\build\Release\publisher.exe localhost:8883 station-01 ..\..\..\station-01.lic ..\..\..\deploy\certs\ca.crt
```

## Integration contract (details in docs/MANUAL.md)

| What            | Value                                              |
| --------------- | -------------------------------------------------- |
| Broker          | `ssl://<devlogd-host>:8883` (TLS, trust the CA)    |
| Username        | device id (must equal the license `subject`)       |
| Password        | the signed license token (`.lic` file content)     |
| Topic           | `devlog/v1/<device_id>/<subsystem>`                |
| Payload         | serialized `devicelog.v1.LogEntry`                 |
| Server-owned    | `entry_id`, `ingest_time`, `audit` — leave empty   |
| QoS             | 1 (at-least-once into the service)                 |
