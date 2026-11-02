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
