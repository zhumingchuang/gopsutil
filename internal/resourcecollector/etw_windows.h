// SPDX-License-Identifier: BSD-3-Clause
#include <stdint.h>
typedef struct rr_trace rr_trace;
typedef struct { uint32_t pid,kind; uint64_t birth,bytes; } rr_io_event;
rr_trace* rr_trace_start(uint32_t *status);
uint32_t rr_trace_drain(rr_trace*, rr_io_event*, uint32_t, uint64_t*, uint64_t*, uint32_t*);
void rr_trace_stop(rr_trace*);
