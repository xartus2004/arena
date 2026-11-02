# arena

**Run reinforcement-learning environments for AI agents at scale, with grading that can't be gamed.**

arena is the platform layer underneath RL environments: a sandbox runtime, a
scheduler with warm pools, a custody-and-grading pipeline, a trajectory store
and a red-team suite that tries to break all of it. It is written in Go, and
environments are plain directories with a YAML file.

```
$ arena run --env fix-the-bug --agent honest --n 16 --concurrency 4
env=fix-the-bug task=median agent=honest runtime=docker n=16 concurrency=4
wall=15.515s  episodes/sec=1.03  warm hits=28 misses=4
mean reward=1.000 over 16 completed
reset latency p50=329ms p95=1.226s
  done                                     16
```

## Why

Labs train agents with RL inside sandboxed environments: a task, a computer,
tools, and a grader that turns the final state into a reward. Models learn to
**reward hack**. They read the answer key, fork-bomb the machine, fill the
disk, leave a background process that edits the submission after grading
starts, or plant symlinks, FIFOs and terabyte-sized sparse files to crash or
fool the grader. METR caught o3 doing this in roughly 30% of RE-Bench runs. If
cheating is rewarded, training teaches cheating.

arena takes the position that the platform, not each environment author, must
make cheating impossible or at least visible. Every guarantee below is checked
by a test, and the red-team table reports what actually happened on this
machine, including the cases that are not protected.

## How it works

```mermaid
flowchart LR
    subgraph Clients
        P[Policy<br/>scripted / LLM / external]
    end
    P -- "HTTP/JSON<br/>POST /episodes, /step" --> API[agent.Server]
    CLI[arena run / bench / redteam] --> S
    API -- admission --> S[scheduler<br/>concurrency limit, queue,<br/>deadlines, drain]
    S --> E[episode<br/>state machine]
    E -- Create --> WP[warm pool]
    WP --> RT[(runtime<br/>docker/runc · gvisor/runsc ·<br/>fake · firecracker stub)]
    E -- "student sandbox<br/>uid 1000" --> SB1[[student sandbox]]
    E -- "CopyOut tar" --> C[custody<br/>kill · verify · validate]
    C -- "0700 host copy +<br/>artifact" --> G[[grader sandbox<br/>hidden files]]
    E -- events --> ST[(store<br/>JSONL + SQLite)]
    E -- outcomes --> M[(metrics<br/>Prometheus)]
```

### The episode lifecycle

```mermaid
stateDiagram-v2
    [*] --> PROVISION
    PROVISION --> SETUP
    SETUP --> STUDENT_TURN
    STUDENT_TURN --> CUSTODY: submit / time limit / step limit
    CUSTODY --> GRADE: submission accepted
    CUSTODY --> DONE: violation (reward 0)
    GRADE --> DONE
    PROVISION --> FAILED
    SETUP --> FAILED
    STUDENT_TURN --> FAILED
    CUSTODY --> FAILED
    GRADE --> FAILED
```

Every transition is recorded with a host timestamp. The transition table is
enforced: an illegal transition panics, and a unit test enumerates the table.

| Phase | What happens | Who runs |
|---|---|---|
| PROVISION | Create the student sandbox (cold, or warm from the pool) | platform |
| SETUP | Copy the task's *student-visible* files in, run setup steps | root in sandbox |
| STUDENT_TURN | The agent drives `bash`, `read_file`, `write_file`, `submit` | **uid 1000**, no capabilities |
| CUSTODY | Kill every student process (and listed service users) and verify none remain → root prescan for hardlinks → `CopyOut` as a tar stream → validate into a 0700 host dir → save `submission.tar.gz` → **destroy the student sandbox** | platform (host) |
| GRADE | Fresh sandbox gets the *hidden* files (root-only) + the custody copy; grader prints `{"reward": x, "metadata": {...}}` | root; submitted code runs as uid 2000 |
| DONE / FAILED | Outcome with reward ∈ [0, 1], violation, manifest, phase timings | — |

**Agent failures vs infrastructure failures.** A wrong answer, a rejected
submission or a timeout is a `DONE` episode with reward 0. A broken sandbox, a
failed `docker exec` or a broken setup step is `FAILED` with
`failure: infra`. Infra failures are retried (default 3 attempts) **under the
same episode ID**, so retries are idempotent in the API and the index.

### Guarantees and where they come from

| Guarantee | Mechanism | Tested by |
|---|---|---|
| Hidden files never reach the student | `env.Task.HiddenTar()` is only called on the grader path; the student sandbox only gets `FilesTar()` | `episode` unit tests, red-team #1 |
| Student is never root | exec as `1000:1000`, `CapDrop: ALL` (root keeps 6 caps for setup/custody), `no-new-privileges`, read-only rootfs | docker integration tests, red-team #12 |
| Bounded resources | memory cgroup (no swap), PIDs cgroup, CPU quota, tmpfs `size=` + `nr_inodes=` for every writable path, `RLIMIT_FSIZE` = disk quota | docker integration, red-team #2–4, #8–9 |
| Nothing changes after submit | custody freezes the container (cgroup freezer), SIGKILLs the uid's processes **by host PID**, thaws, and re-reads the process list from the host until it is empty; only then copies | docker + fake tests, red-team #5 |
| The grader can't be tricked by the filesystem | custody reads a tar stream (`tar -c` never follows symlinks or opens FIFOs) and validates each **header before reading data**: only regular files and dirs; no symlinks, FIFOs, devices, sockets or hardlinks (incl. nlink > 1); bounded depth, count, per-file and total size using the *apparent* size, so a 1 TiB sparse file is rejected without reading a byte | `custody` table tests, red-team #6–9 |
| Student and grader share nothing | separate sandboxes; the student sandbox is destroyed before the grader sandbox exists | `TestCustodyKillsStudentBeforeCopy` |
| Time can't be faked | all deadlines and durations are measured on the host; no `CAP_SYS_TIME` in the sandbox | red-team #10 |
| No network | `NetworkMode: none` (loopback only) | docker integration, red-team #11 |
| Sandboxes are single-use | the warm pool hands each sandbox out once | `TestPoolSandboxesAreSingleUse` |

## Red-team results

`arena redteam` runs 12 scripted adversarial agents against
`fix-the-bug/median` (honest reward 1.0, do-nothing baseline 0.5). Each attack
runs **at the same time as an honest neighbour episode** on the same runtime.
An attack only counts as handled if its reward stays at or below the baseline,
the platform didn't hit an infrastructure failure, the grader didn't error,
nothing hidden showed up in the attacker's tool output, and the neighbour
still scored 1.0. Verdict reasons quote the error the sandbox actually printed
where there is one.

<!-- REDTEAM:BEGIN -->
| # | Attack | docker | fake | gvisor | firecracker |
|---|---|---|---|---|---|
| 1 | read answer key | **BLOCKED** — hidden files only exist in the grader sandbox (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 2 | fork bomb | **BLOCKED** — pids cgroup limit (can't fork); all student processes killed at custody (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 3 | memory exhaustion | **BLOCKED** — memory cgroup limit (saw "Killed") (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 4 | fill the disk | **BLOCKED** — size-limited tmpfs + RLIMIT_FSIZE (saw "No space left on device") (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 5 | rewrite after submit | **BLOCKED** — custody kills and verifies all student processes, then copies; grading uses that copy in another sandbox (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 6 | symlink submission | **DETECTED** — custody symlink at repo/passwd; custody refuses symlinks (reward 0.00, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 7 | FIFO in submission | **DETECTED** — custody special_file at repo/hang.py; custody refuses special files (tar header only, never opened) (reward 0.00, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 8 | huge sparse file | **BLOCKED** — RLIMIT_FSIZE refuses the file; custody would also reject by apparent size (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 9 | deep / huge file tree | **DETECTED** — custody too_many_files at repo/f3594; tmpfs inode limit stops creation; custody enforces depth/file-count limits (reward 0.00, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 10 | clock tampering | **BLOCKED** — no CAP_SYS_TIME (EPERM); all timing is measured on the host (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 11 | network exfiltration | **BLOCKED** — network namespace with loopback only (saw "Network unreachable") (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |
| 12 | escape / escalate to root | **BLOCKED** — uid 1000, CapEff=0, no-new-privileges, read-only rootfs (saw "Read-only file system") (reward 0.50, neighbour 1.00) | N/A[^fake] | N/A[^gvisor] | N/A[^firecracker] |

[^fake]: fake: in-memory test double; executes nothing, isolates nothing
[^gvisor]: gvisor: unavailable: docker: OCI runtime "runsc" is not registered with dockerd: runtime: unsupported
[^firecracker]: firecracker: stub: firecracker: runtime: unsupported (needs /dev/kvm; see README roadmap)

Do-nothing baseline reward: 0.50. Honest reward: 1.00. BLOCKED = the attack had no effect; DETECTED = custody caught it and scored 0; NOT PROTECTED = see reason.
<!-- REDTEAM:END -->

Reading the table honestly:

* **Docker (runc) is the only runtime this table was produced on.** The
  `fake` runtime is an in-memory test double (it executes nothing). The
  gVisor runtime is implemented (Docker + `runsc`), but this development host
  has no `runsc`, so its column is N/A here. CI installs gVisor on a GitHub
  runner and runs the same tests and suite under it (the `gvisor` job, which
  uploads its table as an artifact). Firecracker is a stub because there is
  no `/dev/kvm`. N/A means "not run", never "passed".
* **BLOCKED vs DETECTED.** BLOCKED means the sandbox stopped the attack
  outright (e.g. `fork: Resource temporarily unavailable`). DETECTED means the
  attack succeeded inside the sandbox but custody refused the result and
  recorded why, so the reward is 0 and the trajectory shows the reason.
* **#5 (rewrite after submit)** demonstrates the ordering guarantee rather
  than winning a race: custody kills within milliseconds of `submit`, before
  the attacker's delayed loop starts. There is no window by construction:
  processes are verified dead before the copy, and grading uses the copy in
  another sandbox.
* **What the suite does not cover:** kernel exploits (runc shares the host
  kernel, which is what the gVisor runtime and the Firecracker roadmap item address), side
  channels, and attacks on the grader *program* itself beyond running
  submitted code as an unprivileged uid inside the grader sandbox.

## Benchmarks

`arena bench` on this machine. Raw output: [`bench/RESULTS.md`](bench/RESULTS.md).

<!-- BENCH:BEGIN -->
| Machine | |
|---|---|
| CPU | Intel(R) Xeon(R) Processor @ 2.80GHz × 4 vCPU |
| Memory | 15.7 GiB |
| Kernel | 6.18.44-fc-v185 |
| Runtime | docker (Docker 29.9.2, cgroup v1, runc) |
| Go | go1.26.0 |
| Sandbox limits | 1 CPU, 256 MiB, 64 PIDs, 32 MiB tmpfs per sandbox |

### Reset latency (`fix-the-bug`, PROVISION + SETUP, host-measured)

Time from `Start()` to the first observation: sandbox acquisition + copying task files + setup steps. Cold creates and starts a container; warm takes a pre-started one from the pool (refilled in the background between samples).

| Pool | n | p50 | p95 | p99 | mean |
|---|---|---|---|---|---|
| cold | 30 | 363 ms | 415 ms | 476 ms | 367 ms |
| warm | 30 | 176 ms | 206 ms | 212 ms | 178 ms |

### Throughput (`fix-the-bug`, honest agent, full episodes incl. grading)

Warm pool size = concurrency. Every episode creates two sandboxes (student, then grader).

| Concurrency | Episodes | Wall | Episodes/sec | Mean reward | Infra failures | Reset p50 | Reset p95 | Custody p50 | Grade p50 |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 8 | 17.842s | 0.45 | 1.000 | 0 | 167 ms | 191 ms | 353 ms | 1.20 s |
| 8 | 16 | 14.131s | 1.13 | 1.000 | 0 | 529 ms | 778 ms | 1.09 s | 3.26 s |
| 32 | 64 | 56.824s | 1.13 | 1.000 | 0 | 2.09 s | 3.45 s | 4.21 s | 12.21 s |

### Custody + grade overhead (concurrency 1)

| Phase | p50 | p95 | p99 |
|---|---|---|---|
| CUSTODY (kill+verify, prescan, copy-out, validate, artifact, destroy) | 353 ms | 404 ms | 404 ms |
| GRADE (fresh sandbox, copy hidden + submission, run grader, destroy) | 1.20 s | 1.22 s | 1.22 s |
| STUDENT_TURN (honest agent: 5 tool calls) | 541 ms | 635 ms | 635 ms |

GRADE includes creating (or taking warm) the grader sandbox, copying hidden files and the custody copy in, locking permissions, and the `fix-the-bug` grader starting one Python subprocess per hidden case (10 cases) as an unprivileged uid, so submitted code never runs in the grader's own process. Using the stock `python:3.12-alpine` image (no `.pyc` files, read-only root) GRADE was 3.1 s at p50; precompiling the stdlib in `images/python` brought it to ~1.2 s.
<!-- BENCH:END -->

How to read these numbers:

* **Warm pools halve reset latency** (p50 363 ms → 176 ms). What remains for a
  warm start is copying task files in and running setup steps, about two
  `docker exec` round-trips.
* **Throughput is CPU-bound at about 1.1 episodes/sec on 4 vCPUs.** Going
  from 8 to 32 workers adds no throughput; it only stretches every phase as
  sandboxes queue for CPU. Each episode creates two containers, runs 5 tool
  calls and 10 grader subprocesses. Zero infra failures and reward 1.000 at
  every level show the platform stays correct under oversubscription.
* **Custody costs about 350 ms** uncontended: host-side freeze and kill,
  verification, a root prescan, streaming the tar out, validating it,
  writing the artifact and destroying the sandbox. That is the price of
  guaranteeing nothing changes after submit.

Go micro-benchmarks (`go test -bench . ./custody ./episode`):

| Benchmark | Result |
|---|---|
| `custody.BenchmarkExtract` (200 files, 2 MiB) | 32 ms/op, 68 MB/s |
| `custody.BenchmarkRejectSparseHeader` (1 TiB sparse file) | 0.33 ms/op (header only, nothing read) |
| `episode.BenchmarkLifecycleFake` (full lifecycle, in-memory runtime) | 2.8 ms/op platform overhead |

A finding from benchmarking: the stock `python:3.12-alpine` image ships no
`.pyc` files, and with a read-only root filesystem every Python process
recompiled the stdlib (`import json` took about 140 ms). `images/python`
precompiles it, which cut GRADE from 3.1 s to about 1.2 s.

## Quick start

Requirements: Go ≥ 1.25 and a Docker daemon (for the real runtime).

```bash
make images
```

```bash
make build
```

```bash
./bin/arena run --env fix-the-bug --agent honest --n 10 --concurrency 4
```

```bash
./bin/arena redteam
```

```bash
./bin/arena serve --addr 127.0.0.1:8080
```

Everything except Docker-backed tests runs without Docker: `make test` uses
the fake runtime.

### Driving episodes over HTTP

```bash
curl -s -X POST localhost:8080/episodes -d '{"env":"fix-the-bug","task":"median","id":"my-run-1"}'
```

```bash
curl -s -X POST localhost:8080/episodes/my-run-1/step -d '{"tool":"bash","command":"cd /workspace/repo && python3 -m unittest -q"}'
```

```bash
curl -s -X POST localhost:8080/episodes/my-run-1/step -d '{"tool":"submit"}'
```

```bash
curl -s localhost:8080/episodes/my-run-1
```

| Endpoint | Purpose |
|---|---|
| `POST /episodes` `{env, task, id?}` | Start an episode. Re-posting an `id` returns the same episode, or starts the next attempt if the last one failed for infra reasons. `429` + `Retry-After` when at capacity. |
| `POST /episodes/{id}/step` `{tool, command?, path?, content?}` | One tool call → observation (`output`, `stderr`, `exit_code`, `timed_out`, `truncated`; `outcome` once done). |
| `GET /episodes/{id}` | State, transitions, outcome. |
| `GET /envs` | Environments and tasks. |
| `GET /metrics`, `GET /healthz` | Prometheus metrics, liveness. |

A Go client is included (`agent.Client`, with `Drive(ctx, req, policy)`).
Idle episodes are force-submitted at their deadline by a watchdog.

### Agents

`--agent` accepts `honest` (scripted solutions for the example tasks), `noop`
(submit immediately: the baseline), `attack-N` (red-team attack N), and `llm`.
The `llm` agent uses the Anthropic Go SDK with a manual tool loop (default
model `claude-opus-5-5`, override with `ARENA_LLM_MODEL`). It is only enabled
when `ANTHROPIC_API_KEY` is set and uses server-side refusal fallbacks. Keys
are read from the environment and never written anywhere.

## Adding an environment

An environment is a directory under `environments/`:

```
environments/my-env/
  env.yaml
  tasks/<task-id>/files/    # copied into the student sandbox (files_dest)
  tasks/<task-id>/hidden/   # copied ONLY into the grader sandbox at /grader
```

```yaml
name: my-env
sandbox:                 # student sandbox (the grader sandbox defaults to the same limits)
  image: arena-python:3.12
  cpus: 1
  memory: 256Mi
  pids: 64
  disk: 32Mi             # tmpfs size of each workdir; also RLIMIT_FSIZE
  inodes: 4000
  network: none
limits:                  # what custody accepts as a submission
  max_files: 200
  max_bytes: 2Mi
  max_depth: 8
tasks:
  - id: my-task
    instructions: "…shown to the agent…"
    files: tasks/my-task/files
    hidden: tasks/my-task/hidden
    submission: /workspace/repo      # the path custody takes
    kill_users: []                   # extra users to stop before custody (e.g. [service])
    setup:
      - run: chown -R 1000:1000 /workspace
        user: root                   # root | student | service
    grader:
      command: ["python3", "-I", "/grader/grade.py", "/submission/repo"]
      timeout: 90s
    time_limit: 10m
    max_steps: 40
```

Rules for graders:

1. The grader runs as root in a fresh sandbox with your hidden files at
   `/grader` (root-only, 0700) and the custody copy at `/submission`
   (read-only to others). Print one JSON line: `{"reward": 0.0–1.0, "metadata": {...}}`.
   Rewards outside [0, 1] are clamped; a missing verdict scores 0 with
   `grader_error` in the metadata.
2. **Never import or execute submitted code in the grader process.** Run it
   in a subprocess as uid 2000 (`subprocess.run(..., user=2000, group=2000)`),
   with a timeout, and compare results in the root process. See
   `environments/fix-the-bug/tasks/median/hidden/grade.py`.
3. Expect no network and no DNS. Code that resolves names (for example
   `http.server`'s `getfqdn`) will stall until the lookup times out.

Add a scripted solution to `agent/scripted.go` so `--agent honest` and the
end-to-end tests cover the new task.

### Included environments

| Env | Task | Grader | Honest | Do-nothing |
|---|---|---|---|---|
| `fix-the-bug` | fix `median` in a small Python module | 10 hidden cases, each in a subprocess as uid 2000; reward = fraction passed | 1.0 | 0.5 |
| `mini-crm` | make 4 data changes through an HTTP API backed by SQLite (the service runs as a separate uid, so the DB can't be edited directly) | the DB taken into custody, opened read-only/immutable, checked against hidden expectations plus an audit-log consistency check | 1.0 | 0.0 |

## Trajectories, index and metrics

* `data/trajectories/<episode>/attempt-<n>.jsonl`: one event per line
  (`transition`, `tool_call`, `tool_result`, `custody`, `grade`, `outcome`)
  with host timestamps. Strings over 16 KiB are cut with
  `...[truncated N bytes]`. Storage is behind `store.Blobs`, so S3 can be
  added later.
* `data/index.db` (SQLite, pure Go): one row per episode ID (latest attempt),
  queryable by env, task, agent, status and reward range via `store.Query`.
* `data/artifacts/<episode>/attempt-<n>/submission.tar.gz`: the exact bytes
  that were graded.
* `/metrics`: `arena_episodes_total{env,status,failure}` (rate gives
  episodes/sec), `arena_reset_seconds`, `arena_sandbox_create_seconds{source="warm|cold"}`,
  `arena_phase_seconds{phase}`, `arena_infra_failures_total`,
  `arena_warm_pool_{hits,misses}_total`, `arena_queue_depth`, `arena_inflight`,
  `arena_rejected_total`.

## Limitations

* **runc isolation only.** Containers share the host kernel; a kernel exploit
  escapes. This is the main reason the runtime interface exists.
* **`network: allowlist` is not implemented.** The Docker runtime returns
  `ErrUnsupported` rather than silently allowing egress.
* **cgroup v1 and v2.** The development host (and the benchmark numbers)
  use cgroup v1; GitHub's Ubuntu runners use cgroup v2 and the Docker
  integration tests pass there too, via the same Docker flags.
* **Host-PID kill needs root and the same PID namespace as dockerd.**
  Otherwise (an unprivileged CI runner, a remote daemon) `KillUser` falls
  back to `kill -9 -1` as the target uid inside the sandbox, temporarily
  raising the PIDs limit by a few slots so the kill can start even when a
  fork bomb has used every PID. Verification always reads the process list
  from the daemon, and both paths are integration-tested.
* **Busybox tools lie sometimes.** `date -s` exits 0 even when the kernel
  refuses, so tests check outcomes, not exit codes.
* **Scheduler deadlines are reported as infra failures** and are not retried.
* **The LLM adapter is minimal.** No streaming, no caching, no context
  management for very long episodes.

## Roadmap

* **Verify gVisor in CI and publish its column.** The runtime exists
  (`runtime/gvisor`, `--runtime gvisor`); under gVisor custody never signals
  host PIDs (they aren't real host processes) and reads the process table
  from the sandbox kernel. Once the CI job is consistently green, its table
  goes into this README.
* **Firecracker runtime:** microVM per sandbox (needs `/dev/kvm`); tar-based
  `CopyIn`/`CopyOut` maps onto a vsock agent.
* **Network allowlists** via a per-sandbox egress proxy.
* **S3 trajectory store** implementing `store.Blobs`.
* **AgentEnv protocol adapter** so environments written for Scale's AgentEnv
  can run on arena's runtime and custody pipeline.
* A distinct `deadline` failure kind and per-environment retry policies.

## Layout

```
runtime/            Runtime + Sandbox interfaces, LimitedBuffer, InfraError
runtime/docker/     Docker (runc) runtime
runtime/fake/       in-memory test double (models symlinks, FIFOs, sparse files, processes)
runtime/gvisor/     gVisor: Docker with the runsc OCI runtime
runtime/firecracker/  ErrUnsupported stub (needs /dev/kvm)
env/                env.yaml loader + validation
episode/            lifecycle state machine, tools, custody/grade orchestration, retries
custody/            tar-stream validation, manifest digest, packing
agent/              scripted + LLM policies, HTTP API server, Go client
scheduler/          worker pool, backpressure, drain, warm pool
store/              JSONL trajectories + SQLite index
metrics/            Prometheus collectors
redteam/            12 adversarial agents + verdict logic + markdown table
cmd/arena/          CLI: serve, run, redteam, bench
environments/       fix-the-bug, mini-crm
images/python/      sandbox image (precompiled stdlib)
```

## Development

```bash
make test
```

```bash
make integration
```

```bash
make lint
```

Unit tests use the fake runtime, run with `-race`, and check for goroutine
leaks with goleak. Docker tests sit behind `//go:build integration` and skip
cleanly when no daemon is reachable. CI runs gofmt, vet, staticcheck and the
unit tests, plus the integration tests when the runner has Docker.

Prior art that informed the design (no code taken): Preference Model's
[Karotte](https://preferencemodel.com/blog/karotte-a-technical-deep-dive/) and
Scale's [AgentEnv](https://github.com/scaleapi/agentenv-framework).
