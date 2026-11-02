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
- [x] **M5** — `arena bench` → `bench/RESULTS.md` (reset latency cold
  363 ms vs warm 176 ms p50; ~1.1 episodes/sec CPU-bound on 4 vCPUs at
  concurrency 8 and 32 with zero infra failures; custody ~350 ms, grade
  ~1.2 s), Go benchmarks for custody and the lifecycle, README with
  architecture/lifecycle diagrams, guarantee table, live red-team and
  benchmark tables (`scripts/update-readme.py`), environment authoring
  guide, limitations and roadmap. `images/python` (precompiled stdlib) cut
  GRADE from 3.1 s to 1.2 s. Makefile.

## Post-milestone work
- [x] gVisor runtime implemented (`runtime/gvisor`: Docker + `runsc`). Under
  gVisor, custody never signals host PIDs and lists processes from the
  sandbox kernel's /proc. Not runnable on the dev host (no runsc; downloading
  it was not permitted in this session), so a `gvisor` CI job installs runsc
  from Google's apt repo and runs the integration + red-team suites under it
  (`ARENA_DOCKER_RUNTIME=runsc`; experimental / continue-on-error).
- [x] CI actions bumped to Node 24 versions; README cgroup v1/v2 note fixed.

- [x] gVisor verified in CI (run #12): all integration tests pass and the
  red-team suite is 8 BLOCKED / 4 DETECTED / 0 NOT PROTECTED
  (`docs/REDTEAM-gvisor.md`). Fixes found along the way: sandbox OOM deaths
  are classified as `resource_exhaustion` (agent failure, not infra); under
  gVisor the host PIDs cgroup is a generous ceiling and RLIMIT_NPROC is set
  per exec; no loopback under gVisor (mini-crm skips there). The job is no
  longer continue-on-error.

## Next (suggested follow-ups)
1. Loopback under gVisor so mini-crm can run there.
2. Network allowlist via a per-sandbox egress proxy.
3. S3 implementation of `store.Blobs`; AgentEnv protocol adapter.
4. A distinct `deadline` failure kind.

To resume on a fresh machine: start dockerd if needed, `make images`,
`make test`, `make integration`, `make redteam`, `make bench`,
`python3 scripts/update-readme.py`.

## Known gaps
- CI (GitHub runners run as an unprivileged user) exposed that host-PID
  `KillUser` silently got EPERM. Host signalling is now used only as root;
  otherwise `KillUser` kills from inside the sandbox as the target uid after
  temporarily raising the PIDs limit. Both modes are integration-tested.
- Found under parallel integration load: the cgroup v1 freezer sometimes
  fails to freeze a container mid-fork-bomb ("unable to freeze"). `KillUser`
  now kills by host PID without freezing in that case and keeps looping
  until the host-side process list is empty (verified with repeated parallel
  runs).
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
