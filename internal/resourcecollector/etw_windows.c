// SPDX-License-Identifier: BSD-3-Clause
#define _WIN32_WINNT 0x0602
#include <windows.h>
#include <evntrace.h>
#include <evntcons.h>
#include <tdh.h>
#include <stdlib.h>
#include <string.h>
#include "etw_windows.h"

#define RR_RING 65536
struct rr_trace { TRACEHANDLE session,consumer; HANDLE thread; CRITICAL_SECTION lock; EVENT_TRACE_PROPERTIES *properties; rr_io_event events[RR_RING]; uint32_t count,ended,consumer_status; uint64_t lost,traceLost,unattributed; WCHAR name[64]; };
// A fixed component GUID, distinct from SystemTraceControlGuid. Windows rejects
// SystemTraceControlGuid with any name other than NT Kernel Logger.
static const GUID rr_system={0x620deb21,0x7c92,0x481c,{0xa7,0xe6,0x4d,0x18,0x86,0x8c,0x67,0x27}};
static const GUID rr_disk={0x3d6fa8d4,0xfe05,0x11d0,{0x9d,0xda,0x00,0xc0,0x4f,0xd7,0xba,0x7c}};
static const GUID rr_tcp={0x9a280ac0,0xc8e0,0x11d1,{0x84,0xe2,0x00,0xc0,0x4f,0xb9,0x98,0xa2}};
static const GUID rr_udp={0xbf3a50c5,0xa9c9,0x4988,{0xa0,0x05,0x2d,0xf0,0xb7,0xc8,0x0f,0x80}};
static uint64_t ft(FILETIME f){return ((uint64_t)f.dwHighDateTime<<32)|f.dwLowDateTime;}
static int property(EVENT_RECORD *r,WCHAR *name,uint32_t *value){PROPERTY_DATA_DESCRIPTOR p={0};p.PropertyName=(ULONGLONG)(ULONG_PTR)name;p.ArrayIndex=ULONG_MAX;ULONG size=0;if(TdhGetPropertySize(r,0,NULL,1,&p,&size)!=ERROR_SUCCESS||size!=4)return 0;return TdhGetProperty(r,0,NULL,1,&p,4,(PBYTE)value)==ERROR_SUCCESS;}
static void WINAPI event_record(EVENT_RECORD *r){
  rr_trace *t=(rr_trace*)r->UserContext;uint32_t kind=0,pid=0,bytes=0;UCHAR op=r->EventHeader.EventDescriptor.Opcode;int disk=IsEqualGUID(&r->EventHeader.ProviderId,&rr_disk);int net=IsEqualGUID(&r->EventHeader.ProviderId,&rr_tcp)||IsEqualGUID(&r->EventHeader.ProviderId,&rr_udp);
  if(disk&&(op==10||op==11)){kind=op==10?1:2;uint32_t tid=0;if(!property(r,L"TransferSize",&bytes)||!property(r,L"IssuingThreadId",&tid))goto unknown;HANDLE th=OpenThread(THREAD_QUERY_LIMITED_INFORMATION,FALSE,tid);if(!th)goto unknown;FILETIME b,e,k,u;int valid=GetThreadTimes(th,&b,&e,&k,&u)&&ft(b)<=(uint64_t)r->EventHeader.TimeStamp.QuadPart;pid=valid?GetProcessIdOfThread(th):0;CloseHandle(th);if(!pid)goto unknown;
  }else if(net&&(op==10||op==11||op==26||op==27)){kind=(op==10||op==26)?4:3;if(!property(r,L"PID",&pid)||!property(r,L"size",&bytes))goto unknown;
  }else return;
  {HANDLE ph=OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION,FALSE,pid);if(!ph)goto unknown;FILETIME b,e,k,u;int valid=GetProcessTimes(ph,&b,&e,&k,&u);CloseHandle(ph);if(!valid||ft(b)>(uint64_t)r->EventHeader.TimeStamp.QuadPart)goto unknown;rr_io_event event={pid,kind,ft(b),bytes};EnterCriticalSection(&t->lock);if(t->count<RR_RING)t->events[t->count++]=event;else t->lost++;LeaveCriticalSection(&t->lock);}return;
unknown:EnterCriticalSection(&t->lock);t->unattributed++;LeaveCriticalSection(&t->lock);
}
static ULONG WINAPI buffer_callback(EVENT_TRACE_LOGFILEW *log){rr_trace *t=(rr_trace*)log->Context;EnterCriticalSection(&t->lock);if(log->EventsLost>t->traceLost)t->traceLost=log->EventsLost;LeaveCriticalSection(&t->lock);return TRUE;}
static DWORD WINAPI process_trace(LPVOID value){rr_trace *t=(rr_trace*)value;ULONG status=ProcessTrace(&t->consumer,1,NULL,NULL);EnterCriticalSection(&t->lock);t->consumer_status=status;t->ended=1;LeaveCriticalSection(&t->lock);return 0;}
rr_trace* rr_trace_start(uint32_t *status){
  rr_trace *t=(rr_trace*)calloc(1,sizeof(rr_trace));if(!t){*status=ERROR_OUTOFMEMORY;return NULL;}InitializeCriticalSection(&t->lock);wcscpy(t->name,L"RenderLauncher.Resources.v1");ULONG size=sizeof(EVENT_TRACE_PROPERTIES)+sizeof(t->name);t->properties=(EVENT_TRACE_PROPERTIES*)calloc(1,size);if(!t->properties){*status=ERROR_OUTOFMEMORY;goto fail;}
  EVENT_TRACE_PROPERTIES *p=t->properties;p->Wnode.BufferSize=size;p->Wnode.Guid=rr_system;p->Wnode.ClientContext=1;p->Wnode.Flags=WNODE_FLAG_TRACED_GUID;p->BufferSize=64;p->MinimumBuffers=16;p->MaximumBuffers=64;p->FlushTimer=1;p->LogFileMode=EVENT_TRACE_REAL_TIME_MODE|EVENT_TRACE_SYSTEM_LOGGER_MODE;p->EnableFlags=EVENT_TRACE_FLAG_DISK_IO|EVENT_TRACE_FLAG_NETWORK_TCPIP;p->LoggerNameOffset=sizeof(EVENT_TRACE_PROPERTIES);memcpy((BYTE*)p+p->LoggerNameOffset,t->name,sizeof(t->name));
  *status=StartTraceW(&t->session,t->name,p);if(*status!=ERROR_SUCCESS)goto fail;
  EVENT_TRACE_LOGFILEW log={0};log.LoggerName=t->name;log.ProcessTraceMode=PROCESS_TRACE_MODE_REAL_TIME|PROCESS_TRACE_MODE_EVENT_RECORD;log.EventRecordCallback=event_record;log.BufferCallback=buffer_callback;log.Context=t;t->consumer=OpenTraceW(&log);if(t->consumer==INVALID_PROCESSTRACE_HANDLE){*status=GetLastError();ControlTraceW(t->session,NULL,p,EVENT_TRACE_CONTROL_STOP);goto fail;}
  t->thread=CreateThread(NULL,0,process_trace,t,0,NULL);if(!t->thread){*status=GetLastError();ControlTraceW(t->session,NULL,p,EVENT_TRACE_CONTROL_STOP);CloseTrace(t->consumer);goto fail;}*status=0;return t;
fail:free(t->properties);DeleteCriticalSection(&t->lock);free(t);return NULL;
}
uint32_t rr_trace_drain(rr_trace *t,rr_io_event *out,uint32_t capacity,uint64_t *lost,uint64_t *unknown,uint32_t *status){ULONG query=ControlTraceW(t->session,NULL,t->properties,EVENT_TRACE_CONTROL_QUERY);EnterCriticalSection(&t->lock);uint32_t count=t->count<capacity?t->count:capacity;memcpy(out,t->events,count*sizeof(rr_io_event));if(count<t->count)t->lost+=t->count-count;t->count=0;uint64_t kernelLost=query==ERROR_SUCCESS?(uint64_t)t->properties->EventsLost+t->properties->RealTimeBuffersLost:0;if(kernelLost>t->traceLost)t->traceLost=kernelLost;*lost=t->lost+t->traceLost;*unknown=t->unattributed;*status=query!=ERROR_SUCCESS?query:t->ended?(t->consumer_status?t->consumer_status:ERROR_PROCESS_ABORTED):0;LeaveCriticalSection(&t->lock);return count;}
void rr_trace_stop(rr_trace *t){if(!t)return;ControlTraceW(t->session,NULL,t->properties,EVENT_TRACE_CONTROL_STOP);CloseTrace(t->consumer);WaitForSingleObject(t->thread,INFINITE);CloseHandle(t->thread);free(t->properties);DeleteCriticalSection(&t->lock);free(t);}
