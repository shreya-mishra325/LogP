# Milestone 1 implementation notes

## Scheduling and recovery

One Go goroutine owns the mutable job and session registries. An HTTP request or socket reader submits a command to a 64-entry mailbox. Workers announce total free slots, and the engine subtracts outstanding reservations from negotiated capacity by clamping that announcement to the server's own count of remaining slots. Sending an assignment consumes one free slot. Heartbeats update session liveness, and a separate server clock governs task deadlines.

The ready queue contains tile indices and is bounded by the number of tiles. A lease is indexed by its random ID and is valid only for the matching job, task and live session. Expiry runs before each command and on a 100 ms tick. At the deadline a result is stale, even if it arrives before the next tick. Acceptance removes the lease and appends the validated tile in one state-loop command. Duplicate, expired, wrong-job, wrong-task and wrong-session submissions all receive a stale acknowledgement. Invalid data for a current lease consumes an attempt and triggers recovery. After the third unsuccessful attempt the whole job fails.

Pause revokes outstanding leases. A disconnect closes the session and requeues outstanding work. A stalled writer has a two-second write deadline, and a full 16-message outbound queue causes immediate session eviction. The server keeps up to 32 live sessions and the 32 most recent lost session summaries. A browser may close abruptly, so an eight-second heartbeat timeout covers connections that the transport still considers open.

Cancellation and job expiry revoke all leases and freeze the job state. The queue waits for consenting workers until the five-minute job deadline. A restart loses the in-memory registries.

## Admission and output

The coordinator admits one active job and up to 16 total jobs. It reserves the full raw iteration-buffer size at admission from a 64 MiB budget, including for terminal jobs, until eviction at 30 minutes. Metadata, transient HTTP responses and PNG encoding buffers sit outside that budget. The maximum tile count follows from the 2,048-pixel dimension cap and the 16-pixel minimum tile edge. API concurrency is capped at eight, and tile polling returns at most 32 accepted tiles per request. The engine exposes immutable accepted bytes for encoding outside the state loop.

The Go reference and browser use pixel centers and explicit double-precision multiplication steps. Admission also rejects finite coordinate endpoints whose span would overflow during pixel mapping. Floating-point differences near escape boundaries remain possible across runtimes. Pixels inside the iteration cap are black. Escaped pixels use RGB components `(9n, 5n, 13n) mod 256`, with opaque alpha. Structural checks verify byte length and iteration range. Trusted participants are still responsible for correct computation.

## Transport and consent

Operator HTTP requests use Bearer credentials. Worker sockets require an exact HTTP(S) origin match and a separate credential in the first frame within five seconds. Eight unauthenticated upgrades may be pending. The coordinator accepts only the bundled Mandelbrot kernel. The `supported_kernels` array uses versioned names such as `mandelbrot.v1`; assignments carry that name and integer `kernel_version: 1`. No-payload messages accept an omitted payload or an empty JSON object. Diagnostics are bounded and omit submitted data.

On Join the worker page opens a socket. Each assigned slot gets a fresh Dedicated Worker, which is terminated after result, error, cancellation or the watchdog deadline. Pause and Stop terminate all local threads immediately. Stop closes the socket directly so server disconnect handling recovers the leases. The client suppresses new work while hidden and requires manual Resume when visible. Session duration is a browser control, bounded to 1–60 minutes. The coordinator independently enforces heartbeat and job deadlines.

Milestone 1 uses fixed server limits. Loopback is the default. Binding beyond loopback is an explicit choice for trusted LAN experiments, where HTTP exposes credentials and input to anyone who can observe the traffic. Credentials generated at startup are shown only when the CLI detects an interactive terminal. For unattended runs both credentials must be provided via the environment. The result command creates a new file at the explicit output path and refuses to overwrite an existing one.

The only Go dependency is the pinned [coder/websocket module](https://pkg.go.dev/github.com/coder/websocket@v1.8.15), distributed under the ISC licence. Playwright is development-only. The product name comes from the display identity package, which also supplies both page templates and CLI help.
