//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(api *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if api == nil || plugin == nil || api.abi_version != 1 {
		return 1
	}
	C.store_host_api(api)
	host = callHost
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeABIResponse(response, errorEnvelope("plugin_panic", "额度保护插件发生内部错误"))
			code = 1
		}
	}()
	if method == nil || uint64(n) > 32*1024*1024 {
		writeABIResponse(response, errorEnvelope("invalid_request", "插件请求无效"))
		return 1
	}
	var raw []byte
	if request != nil && n > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	out, err := handleMethod(C.GoString(method), raw)
	if err != nil {
		writeABIResponse(response, errorEnvelope("quota_guard_error", err.Error()))
		return 1
	}
	writeABIResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, n C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	active.Lock()
	e := active.e
	active.e = nil
	active.Unlock()
	if e != nil {
		close(e.stop)
		<-e.done
	}
}

func writeABIResponse(response *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func callHost(method string, payload any, dest any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	ptr := C.CBytes(raw)
	defer C.free(ptr)
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, (*C.uint8_t)(ptr), C.size_t(len(raw)), &response)
	if response.ptr == nil {
		return fmt.Errorf("host callback unavailable: %s (%d)", method, int(code))
	}
	defer C.free_host_buffer(response.ptr, response.len)
	if uint64(response.len) > 32*1024*1024 {
		return fmt.Errorf("host callback response too large: %s", method)
	}
	var env envelope
	if err = json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &env); err != nil {
		return err
	}
	if !env.OK {
		return fmt.Errorf("host callback failed: %s", method)
	}
	return json.Unmarshal(env.Result, dest)
}
