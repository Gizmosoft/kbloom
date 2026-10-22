# kbloom

![kbloom](kbloom%20banner.png)

**kbloom** is a standalone, in-memory Bloom filter microservice. It speaks a Redis-compatible RESP2 subset over TCP, so any standard Redis client (`redis-cli`, redis-py, Jedis, ioredis, and others) can use it without a custom SDK.

Default listen address: `:6380`. 

## Features

- **Language-agnostic API** — Redis protocol clients work out of the box
- **Space-efficient filters** — bit arrays packed in `[]uint64`
- **Optimal sizing** — `BF.RESERVE` derives `m` and `k` from capacity and false-positive rate
- **Fast hashing** — MurmurHash3 (128-bit) with Kirsch–Mitzenmacher double hashing
- **Concurrent** — per-filter `sync.RWMutex`; registry is process-wide
- **Operational defaults** — connection limits, memory caps, idle timeouts, structured logs on stderr
- **Container-ready** — multi-stage distroless image, non-root user, built-in health check

## Protocol

| Command | Description | Reply |
| --- | --- | --- |
| `BF.RESERVE key error_rate capacity` | Create a filter | `+OK` or error if the key exists |
| `BF.ADD key item` | Insert an item | `:1` if a bit flipped, `:0` if probably already present |
| `BF.EXISTS key item` | Membership test | `:0` if definitely absent, `:1` if probably present |
| `PING` `[message]` | Health / handshake | `+PONG` or bulk echo of `message` |

Also accepted for Redis client handshakes: `QUIT`, `ECHO`, `SELECT 0`, and `CLIENT SETNAME` / `GETNAME` / `SETINFO`.

Errors are RESP error replies (`-ERR ...`), which Redis clients surface as exceptions.

> **Persistence:** filters live only in process memory. A restart or container recreate clears all keys. Clients must `BF.RESERVE` and re-add items after startup.

## Requirements

- [Go](https://go.dev/dl/) 1.23 or newer (for local builds)
- Optional: Docker / Docker Compose (for container runs)
- Optional: `redis-cli` for manual testing

## Quick start

### Standalone binary

```bash
git clone https://github.com/Gizmosoft/kbloom.git
cd kbloom
go build -o kbloom ./cmd/kbloom
./kbloom
```

In another terminal:

```bash
redis-cli -p 6380 PING
redis-cli -p 6380 BF.RESERVE users 0.01 1000
redis-cli -p 6380 BF.ADD users alice
redis-cli -p 6380 BF.EXISTS users alice
```

Expected replies: `PONG`, `OK`, `(integer) 1`, `(integer) 1`.

Run without writing a binary:

```bash
go run ./cmd/kbloom
```

### Docker Hub

Published image: [`gizmosoft/kbloom`](https://hub.docker.com/r/gizmosoft/kbloom)

```bash
docker pull gizmosoft/kbloom:latest
docker run --rm -p 6380:6380 gizmosoft/kbloom:latest
```

Override settings with environment variables:

```bash
docker run --rm -p 6380:6380 \
  -e KBLOOM_LOG_LEVEL=debug \
  -e KBLOOM_MAX_CLIENTS=500 \
  gizmosoft/kbloom:latest
```

### Docker (build locally)

With Compose (builds and starts on port `6380`):

```bash
docker compose up --build
# or: docker-compose up --build
```

Manual build and run:

```bash
docker build -t kbloom:local .
docker run --rm -p 6380:6380 kbloom:local
```

Logs go to stderr (`docker logs <container>`). The image health check runs `kbloom health` (a local `PING`).

## Configuration

Flags win over environment variables. Omitting both uses the default.

| Flag | Environment | Default | Description |
| --- | --- | --- | --- |
| `-addr` | `KBLOOM_ADDR` | `:6380` | TCP listen address |
| `-max-bits` | `KBLOOM_MAX_BITS` | `4294967296` | Max bits per filter (`0` = unlimited) |
| `-max-memory` | `KBLOOM_MAX_MEMORY` | `1073741824` | Total byte budget for all filters (`0` = unlimited) |
| `-max-clients` | `KBLOOM_MAX_CLIENTS` | `10000` | Concurrent connections (must be &gt; 0) |
| `-timeout` | `KBLOOM_TIMEOUT` | `0` | Idle read timeout (`0` disables) |
| `-write-timeout` | `KBLOOM_WRITE_TIMEOUT` | `10s` | Write timeout (`0` disables) |
| `-log-level` | `KBLOOM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

Timeouts use Go duration strings (`10s`, `500ms`, `2m`).

```bash
./kbloom -addr 127.0.0.1:6380 -max-clients 500 -log-level debug
KBLOOM_ADDR=127.0.0.1:6380 KBLOOM_MAX_CLIENTS=500 ./kbloom
```

No `.env` file is required or loaded. Set variables in the shell, process manager, or Compose `environment` block.

## Client examples

**Python (redis-py)**

```python
import redis

r = redis.Redis(host="127.0.0.1", port=6380)
r.execute_command("BF.RESERVE", "users", 0.01, 1_000_000)
r.execute_command("BF.ADD", "users", "alice")
print(r.execute_command("BF.EXISTS", "users", "alice"))  # 1
```

**Node.js (ioredis)**

```js
const Redis = require("ioredis");
const r = new Redis(6380);
await r.call("BF.RESERVE", "users", "0.01", "1000000");
await r.call("BF.ADD", "users", "alice");
console.log(await r.call("BF.EXISTS", "users", "alice")); // 1
```

## Architecture

```
Client (Redis library)
        │  TCP / RESP2
        ▼
  net.Listen (:6380)
        │  one goroutine per connection
        ▼
  RESP parser  →  command dispatch
        │
        ▼
  Store (map[string]*Filter)
        │
        ▼
  Bloom filter ([]uint64 + MurmurHash3)
```

Sizing on `BF.RESERVE`:

\[
m = \left\lceil -\frac{n \ln p}{(\ln 2)^2} \right\rceil,\quad
k = \left\lfloor \frac{m}{n}\ln 2 + 0.5 \right\rfloor
\]

Package layout:

| Path | Role |
| --- | --- |
| `cmd/kbloom` | Process entrypoint, config, health probe |
| `internal/server` | TCP accept loop and command handlers |
| `internal/resp` | RESP2 reader/writer |
| `internal/bloom` | Filter, registry, sizing |
| `internal/murmur3` | MurmurHash3 x64 128-bit |

## Development

```bash
go test ./...
go test -race ./...
go test -fuzz=FuzzReadCommand -fuzztime=5s ./internal/resp
```

Build a release-style Linux binary (as used in the Docker image):

```bash
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o kbloom ./cmd/kbloom
```

## Contributing

Contributions are welcome.

1. Fork the repository and create a topic branch from `main`.
2. Keep changes focused; match existing style and package boundaries.
3. Add or update tests for behavior changes.
4. Run `go test ./...` (and `-race` for concurrency-related changes).
5. Open a pull request with a clear description of the problem and solution.

Please open an issue first for large design changes (persistence, replication, or new commands) so the approach can be discussed before implementation.

## License

[MIT](LICENSE) © Kartikey Hebbar
