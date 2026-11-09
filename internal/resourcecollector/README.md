# Modern process resource collector

This is the native node and process collector for the modern RenderServer resource protocol.
It does not change the existing gopsutil monitor exports or load licensing code.
Supported build target: Windows x64, Go 1.24+ with cgo and a matching C compiler.

Implemented: process enumeration, birth identity, per-window CPU, working set,
private commit, private working set, generic I/O, per-process WDDM GPU engines and
GPU memory, frozen pagination, a single-process query and current process-tree
groups. Node CPU/memory, volume capacity and caller space, volume/disk mapping,
physical disk busy/read/write rates, per-interface receive/transmit rates and
WDDM adapter usage/memory are included. Process disk and TCP/UDP IPv4/IPv6
traffic use a component-owned ETW system logger. The Launcher owns an isolated
native child and forwards complete snapshots through authenticated Server and
Manager resource messages. Tree disk/network/GPU aggregates are included.
Job/session membership and Manager UI/history are separate work. Generic I/O
is never returned as disk or network activity.

## Build and verify

From this repository:

```powershell
./scripts/build-render-resources.ps1
go run ./cmd/render-resources-probe
go test -race ./internal/resourcecollector
go vet ./internal/resourcecollector ./cmd/render-resources
```

From RenderLauncher, using its own installed koffi dependency:

```powershell
node test/resource-dll-smoke.cjs ../gopsutil/build/render-resources/render_resources.dll
```

The development output is `build/render-resources/render_resources.dll`, with a
generated C header and source/hash manifest. Outputs are ignored. No retired DLL
paths or release filters are changed. The probe does not create traffic, render
sessions, services or privilege changes. The smoke test creates a hidden isolated
Node child, reads the real DLL and keeps the Go runtime loaded until child exit.

## ABI 1

`RR_ABIVersion()` returns 1. Other functions return an explicit numeric status.

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Invalid argument or query |
| 2 | Monitor not running |
| 3 | First snapshot not ready |
| 4 | Snapshot expired or evicted |
| 5 | Process not found in the snapshot |
| 6 | Expected process identity changed |
| 7 | Result/resource limit exceeded |
| 8 | Internal collection/query error |
| 9 | Output buffer too small |
| 10 | Result handle expired or released |
| 11 | Monitor already running |

1. Call `RR_StartProcessMonitor(nodeId, utf8Length)`. Node IDs are 1–128 ASCII
   letters/digits/underscore/hyphen and must start with a letter or digit.
2. Call `RR_GetNodeUsage`, `RR_GetProcessCapabilities`, `RR_ListProcessUsage`, `RR_GetProcessUsage`
   or `RR_GetProcessGroupUsage` with an output `uint64_t resultId` pointer. Query
   functions take UTF-8 JSON and its byte length (maximum 4096 bytes).
3. Call `RR_ReadResult(resultId, NULL, 0, &requiredBytes)`. Code 9 returns the
   required length **including the trailing NUL**, without writing a partial JSON.
4. Allocate that buffer, call `RR_ReadResult` again and parse the UTF-8 JSON.
   Both reads use exactly the frozen result bytes, even if sampling advances.
5. Call `RR_ReleaseResult`. At most eight pending results, each alive for at most
   15 seconds; handles are not reused after stop/start in the same DLL instance.
6. Call `RR_StopProcessMonitor` before normal child exit. Never unload a running
   Go c-shared library. Native calls belong in the owned isolated worker, not
   the Launcher event loop or Manager process.

List example: `{"limit":100,"sortBy":"cpu","descending":true}`.
Supported sort fields are pid/name/cpu/memory/gpu/disk/network; disk and network
sort by the sum of directional B/s, missing values last. Memory compares the
original uint64 byte values without a float conversion. Follow
`nextCursor` with exactly the same query parameters. Each encoded page is at most
64 KiB; limit is an upper bound, not a guaranteed row count. The current and prior
snapshot are retained, each at most 15 seconds; eviction returns code 4. Both
`enumerationComplete` and per-field availability must be checked. Truncated
snapshots explicitly report the row/byte limit; all rows are never silently
replaced by a top-N summary.

Single-process example: `{"pid":123,"expectedProcessKey":"..."}`. Optionally
pin `snapshotId` to the same list generation. A PID-only call queries the latest
cached generation, not a fresh OS lookup. Consumers must inspect its sample time.
Process keys include node ID, collector epoch, PID and original 100 ns FILETIME.
Keys intentionally change after collector restart; callers must establish a new
watch and baseline rather than join identities across epochs.

Tree example: `{"rootProcessKey":"...","snapshotId":"..."}`. A parent's
birth must precede its child; this is a snapshot of verifiable current ancestry,
not permanent Job/session membership. Unknown/incomplete CPU or memory coverage
returns a partial group with null aggregate. GPU sums only aligned physical
engine instances and then selects the busiest engine. Unknown members/windows
produce a partial aggregate. Shared GPU allocations can occur in multiple
processes, so group GPU memory is a sum of attributed allocations, not physical
device occupancy; node GPU memory uses the adapter counters instead.
`parentVerified=false` prevents using a possibly recycled enumeration entry as
ancestry proof; such potential members make `membershipComplete=false`.

Byte counters and FILETIME values are decimal strings. CPU percent is a share of
all active logical processors across processor groups; `corePercent` uses one
logical CPU as 100%. The first sample and counter/topology discontinuities warm
up. Protected processes remain as rows with null values and explicit statuses.

Private working set is read from `PROCESS_MEMORY_COUNTERS_EX2` on Windows 10
22H2 with the September 2023 cumulative update or newer supported Windows 11
builds. On older/unverified builds, working set and commit still have their own
semantics, while private working set remains null. It is never replaced by
commit or the full working set. The native structure avoids matching duplicate
legacy Process performance-counter instance names.

GPU uses a persistent English PDH query and two valid samples. Each instance is
joined to a verified process birth window. The most active engine represents the
process GPU percentage; engines/GPUs are not summed. No GPU instance is reported
as unknown, rather than an invented zero; a valid sampled idle engine may be 0.
Per-process GPU memory includes shareable allocations and must not be summed to
derive total/free physical GPU memory.

Sources: [memory counters EX2](https://learn.microsoft.com/en-us/windows/win32/api/psapi/ns-psapi-process_memory_counters_ex2),
[PDH wildcard arrays](https://learn.microsoft.com/en-us/windows/win32/api/pdh/nf-pdh-pdhgetformattedcounterarrayw),
[GPU aggregation](https://devblogs.microsoft.com/directx/gpus-in-the-task-manager/).

## ETW and deployment

ETW uses the fixed `RenderLauncher.Resources.v1` logger and a component GUID
distinct from SystemTraceControlGuid. It never takes over NT Kernel Logger,
stops another session, modifies group membership or elevates. Deploy with the
Windows profile privilege and required ETW ACLs, or an appropriately privileged
administrator/service identity. Start failures preserve their native error;
access denied reports `permission_denied` and null disk/network values while
CPU/memory/GPU/node metrics continue. A conflicting logger reports
`ETW_SESSION_ALREADY_EXISTS`; investigate and clean up a verified orphan before
restarting. This implementation does not install an elevated collection service.

Disk completion events use TDH `IssuingThreadId` and `TransferSize`, verify the
thread birth against event time, then resolve process birth. Network events use
payload PID and size; only send/receive opcodes count (no retransmit/connect
double counting). A matching PID and birth are required. The bounded event ring
and ETW loss counters cause explicit partial windows. Unknown attribution never
becomes fabricated zero. Disk/network totals start at this collector epoch;
after any lost/unattributed event totals become null until restart. Subsequent
healthy window rates may be available with partial status and
`ETW_TOTALS_INCOMPLETE`. Generic I/O remains process-lifetime OS counters.

Normal stop closes only this instance's trace handle and waits for its consumer.
Forced process termination can leave an ETW logger orphan, which is reported
explicitly on restart rather than claiming an active replacement collector.

All byte rates are B/s. Process disk/network percentages have no shared reliable
denominator and are intentionally represented as throughput, as in Task Manager.
Node NIC utilization uses reported directional link bit rate; virtual adapters
are listed individually and are never blindly summed. Volume `usedPercent` is
capacity occupancy, while physical disk `busyPercent` is 100 minus idle time.
Volumes expose OS extent disk numbers; mapping errors are independent of space.

Build the Launcher with `npm run build` from its repository. It builds this modern
DLL from source and copies it into ignored `.native-build`; official packaging
builds it again and ships `native/resources/` with its hash manifest and license.
The official Launcher executable embeds `requireAdministrator` with `uiAccess=false`:
Windows requests UAC at launch and the owned collector inherits the elevated
token. Cancelling UAC prevents startup. Development via `node build/startup.js`
must run from an elevated terminal; Node's own executable is not modified.
The old `gopsutil_monitor.dll` is neither loaded nor shipped by this component.

ETW references: [system logger configuration](https://learn.microsoft.com/en-us/windows/win32/etw/configuring-and-starting-a-systemtraceprovider-session),
[disk events](https://learn.microsoft.com/en-us/windows/win32/etw/diskio-typegroup1),
[network events](https://learn.microsoft.com/en-us/windows/win32/etw/tcpip-typegroup1).
