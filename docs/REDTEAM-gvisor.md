# Red-team results: runc vs gVisor

Produced by the `gvisor` job of CI run
[#12](https://github.com/xartus2004/arena/actions/runs/41475800832) on a
GitHub-hosted Ubuntu runner (cgroup v2, arena running as an unprivileged
user, so custody uses the in-sandbox kill path on both runtimes). Copied
verbatim from that run's output. Two reason texts there were still the
runc-specific templates: under gVisor, #2 is stopped by `RLIMIT_NPROC`
enforced by gVisor's kernel (not the PIDs cgroup), and #11 sees no network
interfaces at all (not "loopback only"). The templates have since been made
runtime-neutral.

| # | Attack | docker (runc) | gvisor (runsc) |
|---|---|---|---|
| 1 | read answer key | **BLOCKED** — hidden files only exist in the grader sandbox (reward 0.50, neighbour 1.00) | **BLOCKED** — hidden files only exist in the grader sandbox (reward 0.50, neighbour 1.00) |
| 2 | fork bomb | **BLOCKED** — pids cgroup limit (can't fork); all student processes killed at custody (reward 0.50, neighbour 1.00) | **BLOCKED** — pids cgroup limit (can't fork); all student processes killed at custody (reward 0.50, neighbour 1.00) |
| 3 | memory exhaustion | **BLOCKED** — memory cgroup limit (saw "Killed") (reward 0.50, neighbour 1.00) | **DETECTED** — custody resource_exhaustion at -; memory cgroup limit (no error text captured) (reward 0.00, neighbour 1.00) |
| 4 | fill the disk | **BLOCKED** — size-limited tmpfs + RLIMIT_FSIZE (saw "No space left on device") (reward 0.50, neighbour 1.00) | **BLOCKED** — size-limited tmpfs + RLIMIT_FSIZE (saw "No space left on device") (reward 0.50, neighbour 1.00) |
| 5 | rewrite after submit | **BLOCKED** — custody kills and verifies all student processes, then copies; grading uses that copy in another sandbox (reward 0.50, neighbour 1.00) | **BLOCKED** — custody kills and verifies all student processes, then copies; grading uses that copy in another sandbox (reward 0.50, neighbour 1.00) |
| 6 | symlink submission | **DETECTED** — custody symlink at repo/passwd; custody refuses symlinks (reward 0.00, neighbour 1.00) | **DETECTED** — custody symlink at repo/stats.py; custody refuses symlinks (reward 0.00, neighbour 1.00) |
| 7 | FIFO in submission | **DETECTED** — custody special_file at repo/hang.py; custody refuses special files (tar header only, never opened) (reward 0.00, neighbour 1.00) | **DETECTED** — custody special_file at repo/hang.py; custody refuses special files (tar header only, never opened) (reward 0.00, neighbour 1.00) |
| 8 | huge sparse file | **BLOCKED** — RLIMIT_FSIZE refuses the file; custody would also reject by apparent size (reward 0.50, neighbour 1.00) | **BLOCKED** — RLIMIT_FSIZE refuses the file; custody would also reject by apparent size (reward 0.50, neighbour 1.00) |
| 9 | deep / huge file tree | **DETECTED** — custody too_many_files at repo/f3594; tmpfs inode limit stops creation; custody enforces depth/file-count limits (reward 0.00, neighbour 1.00) | **DETECTED** — custody resource_exhaustion at -; tmpfs inode limit stops creation; custody enforces depth/file-count limits (reward 0.00, neighbour 1.00) |
| 10 | clock tampering | **BLOCKED** — no CAP_SYS_TIME (EPERM); all timing is measured on the host (reward 0.50, neighbour 1.00) | **BLOCKED** — no CAP_SYS_TIME (EPERM); all timing is measured on the host (reward 0.50, neighbour 1.00) |
| 11 | network exfiltration | **BLOCKED** — network namespace with loopback only (saw "Network unreachable") (reward 0.50, neighbour 1.00) | **BLOCKED** — network namespace with loopback only (saw "Network unreachable") (reward 0.50, neighbour 1.00) |
| 12 | escape / escalate to root | **BLOCKED** — uid 1000, CapEff=0, no-new-privileges, read-only rootfs (saw "Read-only file system") (reward 0.50, neighbour 1.00) | **BLOCKED** — uid 1000, CapEff=0, no-new-privileges, read-only rootfs (saw "Read-only file system") (reward 0.50, neighbour 1.00) |

Do-nothing baseline reward: 0.50. Honest reward: 1.00.

**Totals:** runc 9 BLOCKED / 3 DETECTED; gVisor 8 BLOCKED / 4 DETECTED;
NOT PROTECTED on neither.

## What differs under gVisor, and why

* **#3 memory exhaustion and #9 deep tree are DETECTED, not BLOCKED.** gVisor's
  user-space kernel is charged to the sandbox's memory, so a process that
  grabs memory (or millions of inodes on tmpfs) gets the whole sandbox
  OOM-killed rather than just itself. arena sees Docker's OOM flag, classifies
  this as `resource_exhaustion` (the agent's doing, reward 0, never retried as
  infrastructure) and takes nothing into custody.
* **#2 fork bomb** needed two adjustments, found by this CI job: Docker's PIDs
  cgroup counts gVisor's own host threads (a low limit killed the sandbox
  kernel), and container-level rlimits don't reach `docker exec` processes.
  Under gVisor arena keeps a generous host PIDs ceiling and every unprivileged
  command sets `RLIMIT_NPROC` itself, which gVisor's kernel enforces per
  sandbox.
* **No loopback.** With `network: none`, gVisor exposes no network interfaces
  at all, so `mini-crm` (an in-sandbox HTTP service) can't run there; its
  end-to-end test skips under gVisor with that reason.
* **Custody never signals host PIDs under gVisor.** Processes inside gVisor
  are not host processes, so arena lists and kills them through the sandbox's
  own kernel.
