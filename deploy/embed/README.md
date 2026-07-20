# Embedding devlogd in your C++ application (.deb deployment)

This directory shows how your C++ media-sanitization app **starts devlogd as it
initializes** and shuts it down cleanly on exit, all inside one `.deb`.

- [`devlogd_supervisor.hpp`](devlogd_supervisor.hpp) / [`.cpp`](devlogd_supervisor.cpp)
  — a dependency-free (`POSIX` + libstdc++) supervisor: `start()` spawns
  `devlogd` and blocks until its `/readyz` endpoint returns `200`; `stop()`
  drains it gracefully (`SIGTERM` → wait → `SIGKILL`). The destructor stops it.
- [`example_main.cpp`](example_main.cpp) — the entire integration in ~40 lines.
- [`CMakeLists.txt`](CMakeLists.txt) — build the library into your app.

```cpp
devlog::SupervisorOptions opts;
opts.binary_path = "/opt/devlog/bin/devlogd";
opts.config_path = "/etc/devlog/devlogd.yaml";
devlog::DevlogdSupervisor devlogd(opts);
devlogd.start();          // throws if devlogd can't reach Redis/MinIO or become ready
// … run your application; devlogd.running() tells you if it died …
devlogd.stop();           // graceful drain
```

**Why spawn (not a separate systemd unit)?** You chose lifecycle coupling: the
audit logger lives and dies with the app, works on hosts without systemd (field
robots, minimal containers), and needs no cross-unit ordering. The trade-off is
that your app owns supervision — `running()` lets you react if devlogd dies. If
you later want independent restarts, the same binary drops into a systemd unit
unchanged (it already handles `SIGTERM` and `/readyz`).

## Startup ordering & fail-fast

devlogd validates everything at boot and **exits non-zero** on bad config,
unreachable Redis/MinIO, or missing keys. `start()` surfaces that as an
exception (it detects an early child exit and does not hang). Decide your
policy in `example_main.cpp`:

- **Compliance-critical** (default here): if audit logging can't start, abort
  the app — you must not sanitize media you can't prove you sanitized.
- **Best-effort**: log a warning and continue; retry `start()` later.

Redis and MinIO must be reachable before `start()`. On a single appliance,
ship them in the same `.deb`/host and mark them as dependencies (below), or
point the config at managed endpoints via `DEVLOG_REDIS_ADDR` / `DEVLOG_S3_*`.

## `.deb` layout

Bundle the static Go binary (built once, `CGO_ENABLED=0`) and its runtime
material alongside your app:

```
/opt/devlog/bin/devlogd                 # static binary (from: go build ./cmd/devlogd)
/etc/devlog/devlogd.yaml                # config (from config/devlogd.example.yaml)
/etc/devlog/certs/{ca.crt,server.crt,server.key}
/etc/devlog/keys/{signing.key,issuer.pub}   # NOTE: no issuer.key on the appliance
/var/lib/devlog/                        # working dir
/var/log/devlog/                        # devlogd.log (if you redirect output)
```

Build the binary for the package (in CI, from the repo root):

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o pkgroot/opt/devlog/bin/devlogd ./cmd/devlogd
```

### `debian/control` (excerpt)

```
Package: your-sanitizer-app
Depends: ${shlibs:Depends}, redis-server, minio | ca-certificates
Description: Media sanitization station with embedded audit logging (devlogd)
```

### `debian/your-app.install`

```
pkgroot/opt/devlog/bin/devlogd            opt/devlog/bin/
pkgroot/etc/devlog/devlogd.yaml           etc/devlog/
pkgroot/etc/devlog/certs/*                etc/devlog/certs/
pkgroot/etc/devlog/keys/*                 etc/devlog/keys/
```

### `debian/postinst` (permissions are the security boundary)

```sh
#!/bin/sh
set -e
# Dedicated, unprivileged service identity.
adduser --system --group --home /var/lib/devlog --no-create-home devlog 2>/dev/null || true
install -d -o devlog -g devlog -m 0750 /var/lib/devlog /var/log/devlog
# The signing key is the crown jewel — readable only by the service.
chown -R devlog:devlog /etc/devlog/keys /etc/devlog/certs
chmod 0600 /etc/devlog/keys/signing.key /etc/devlog/certs/server.key
chmod 0644 /etc/devlog/keys/issuer.pub /etc/devlog/certs/ca.crt /etc/devlog/certs/server.crt
#DEBHELPER#
```

Run your C++ app (and thus devlogd) as the `devlog` user, or grant that user
read access to the keys. **Never ship `issuer.key`** — licenses are minted on a
separate machine; the appliance needs only `issuer.pub` to verify them.

### `debian/prerm`

Nothing devlogd-specific is required: your app owns devlogd's lifecycle, so
stopping the app (its `stop()` / destructor) drains devlogd. If your app is a
systemd unit, `systemctl stop` sends `SIGTERM`, which `example_main.cpp`
forwards to the supervisor.

## Try it locally (no packaging)

```bash
cmake -S deploy/embed -B build/embed && cmake --build build/embed
# with the real service from this repo:
DEVLOGD_BIN=$(go env GOPATH)/bin/devlogd DEVLOGD_CFG=$PWD/config/devlogd.yaml \
  ./build/embed/devlogd-supervisor-example
```
