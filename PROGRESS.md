# PROGRESS

## Step 0 — environment report (2026-11-01, development container)

| Probe | Result |
|---|---|
| `go version` | go1.24.7 installed; module needs go ≥ 1.25 for deps, so the `auto` toolchain fetched **go1.26.0** |
| `docker info` | CLI 29.9.2 present; daemon **not running** at first. `dockerd` was installed, so I started it as root → Server 29.9.2, runc 1.5.2, storage overlayfs (containerd snapshotter), **cgroup v1**, cgroupfs driver |
| `ls -l /dev/kvm` | **missing** → no Firecracker / KVM microVMs |
| `id` | `uid=0(root)` |
| `cat /sys/fs/cgroup/cgroup.controllers` | **missing** — host uses cgroup v1 (`cpu, cpuacct, cpuset, memory, devices, freezer, blkio, pids`) with a mostly-empty v2 `unified` mount |
| `which runsc bwrap unshare` | only `/usr/bin/unshare` → no gVisor, no bubblewrap |
| Other | 4 vCPU, 15 GiB RAM, no swap, Linux 6.18 (Firecracker guest), `python:3.12-alpine` pullable (Docker Hub rate-limits intermittently) |

**Decision:** Docker (runc) is the primary runtime, driven via the official Go SDK
(`github.com/docker/docker/client`). Isolation it actually provides here:
separate PID/mount/net/IPC/UTS namespaces, `--network none`, memory + PIDs +
CPU cgroup limits, read-only root filesystem, size- and inode-limited tmpfs for
every writable path (disk quota; overlay `--storage-opt size` needs xfs+pquota
which this host does not have), all capabilities dropped except the handful
root needs for setup/custody, `no-new-privileges`, and the student running as
uid 1000. It does **not** provide a separate kernel: a kernel exploit escapes
runc. gVisor and Firecracker are interface-complete stubs returning
`ErrUnsupported`. A `fake` in-memory runtime is used for unit tests and
provides no isolation at all (its `Capabilities()` are all false).

Notes for anyone reproducing: the Docker daemon is not started automatically in
this container; run `dockerd >/tmp/dockerd.log 2>&1 &` first.

## Milestones

- [x] **M1** — Step 0 report, repo skeleton, `runtime` interface, `fake` runtime,
  gVisor/Firecracker stubs, `env` YAML loader, unit tests, CI workflow.
- [x] **M2** — Docker runtime (read-only rootfs, tmpfs quotas, caps dropped,
  no-new-privileges, host-side freeze+kill for `KillUser`), episode state
  machine with recorded transitions, custody (tar-stream validation:
  symlink/FIFO/device/hardlink/depth/count/apparent-size limits), separate
  grader sandbox, `fix-the-bug` env, honest + noop agents, HTTP API + Go
  client with idempotent retries and backpressure. E2E on Docker: honest = 1.0,
  noop baseline = 0.5.
- [x] **M3** — red-team suite: 12 scripted adversarial agents (`redteam/`),
  each run concurrently with an honest neighbour episode on the same runtime;
  verdicts are judged from evidence (reward vs the do-nothing baseline,
  neighbour reward, grader errors, tool output). `arena redteam` prints the
  attack × runtime table (`docs/REDTEAM.md`). Docker: 9 BLOCKED, 3 DETECTED,
  0 NOT PROTECTED; fake/gvisor/firecracker are reported as N/A with reasons.
- [x] **M4** — `scheduler`: worker pool with a global concurrency limit
  shared with the HTTP API, bounded queue (`ErrQueueFull` / HTTP 429),
  per-episode deadlines, graceful drain on shutdown (and forced cancel on
  timeout, still destroying sandboxes). Warm pool (`scheduler.Pool`) wraps
  any runtime, keeps N single-use sandboxes per spec, refills in the
  background and expires idle ones. `store`: JSONL trajectory per attempt
  (fields > 16 KiB truncated with a marker) behind a `Blobs` interface + a
  SQLite index (modernc.org/sqlite) queryable by env/task/agent/status/reward;
  retries upsert by episode ID. `metrics`: Prometheus counters/histograms.
  `mini-crm` env (SQLite-backed HTTP service run as a separate uid; DB is the
  submission). CLI: `arena run`, `arena serve`. Optional LLM policy
  (`--agent llm`, only with `ANTHROPIC_API_KEY`).
- [ ] M5 — benchmarks + README.

## Next
M5: `arena bench` (episodes/sec at concurrency 1/8/32, cold vs warm reset
p50/p95/p99, custody+grade overhead) → `bench/RESULTS.md`, Go benchmarks,
README (problem, mermaid architecture, red-team + benchmark tables, how to add
an environment, limitations, roadmap).

## Known gaps
- mini-crm setup was 6 s until the server skipped `HTTPServer`'s reverse-DNS
  lookup (`socket.getfqdn`), which stalls without network. Environment authors
  should expect the same from any code that resolves names.
- A per-episode deadline (scheduler) is reported as an infrastructure
  failure and is not retried; there is no separate "deadline" failure kind yet.
- Red-team attack 5 (rewrite after submit): custody kills within milliseconds
  of `submit`, before the attacker's 1 s delay elapses, so the attack shows
  the ordering guarantee (verified kill → copy → separate grader sandbox)
  rather than winning a race. There is no race to win by construction; the
  fake-runtime unit test asserts the same ordering with surviving processes.
- runc shares the host kernel: a kernel exploit is out of scope for the
  Docker runtime and is the reason gVisor/Firecracker are on the roadmap.
- busybox `date -s` exits 0 even when the kernel refuses to set the clock,
  so tests check the clock afterwards rather than trusting exit codes.
- Docker integration tests do not run goleak (the Docker client keeps idle
  HTTP connections); unit tests do.
- No KVM / runsc on the dev host → only runc-level isolation can be demonstrated.
- `network: allowlist` is part of the spec but no runtime enforces it yet (Docker returns ErrUnsupported rather than silently allowing everything).
