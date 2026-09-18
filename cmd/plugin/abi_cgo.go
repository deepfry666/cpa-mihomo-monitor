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
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"cpa-mihomo-monitor/internal/entryplugin"
)

const (
	pluginABIVersion  = 1
	maxMethodBytes    = 128
	maxRequestBytes   = 2 << 20
	maxPluginResponse = 2 << 20
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	runtimeMu   sync.RWMutex
	application = entryplugin.New()
	shutDown    bool
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if host == nil || pluginAPI == nil || host.abi_version != C.uint32_t(pluginABIVersion) {
		return 1
	}
	runtimeMu.Lock()
	application = entryplugin.New()
	shutDown = false
	runtimeMu.Unlock()
	pluginAPI.abi_version = C.uint32_t(pluginABIVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			writeResponse(response, errorEnvelope("plugin_panic", "Mihomo Monitor entry recovered from an internal error"))
			code = 1
		}
	}()
	name, err := boundedCString(method, maxMethodBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("invalid_method", err.Error()))
		return 1
	}
	length, err := boundedLength(uint64(requestLen), maxRequestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("invalid_request", err.Error()))
		return 1
	}
	var rawRequest []byte
	if request != nil && length > 0 {
		rawRequest = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	runtimeMu.RLock()
	app := application
	stopped := shutDown
	runtimeMu.RUnlock()
	if stopped || app == nil {
		writeResponse(response, errorEnvelope("plugin_shutdown", "plugin is not available"))
		return 1
	}
	result, err := app.Handle(name, rawRequest)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	raw, err := json.Marshal(envelope{OK: true, Result: result})
	if err != nil {
		writeResponse(response, errorEnvelope("encode_error", "failed to encode plugin response"))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(pointer unsafe.Pointer, length C.size_t) {
	if pointer != nil {
		C.free(pointer)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	runtimeMu.Lock()
	application = nil
	shutDown = true
	runtimeMu.Unlock()
}

func boundedCString(pointer *C.char, maximum int) (string, error) {
	if pointer == nil {
		return "", fmt.Errorf("method is required")
	}
	base := unsafe.Pointer(pointer)
	for length := 0; length < maximum; length++ {
		if *(*byte)(unsafe.Add(base, length)) == 0 {
			if length == 0 {
				return "", fmt.Errorf("method is required")
			}
			return C.GoStringN(pointer, C.int(length)), nil
		}
	}
	return "", fmt.Errorf("method exceeds %d bytes", maximum)
}

func boundedLength(value uint64, maximum int) (int, error) {
	if value > uint64(maximum) {
		return 0, fmt.Errorf("request exceeds %d bytes", maximum)
	}
	return int(value), nil
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 || len(raw) > maxPluginResponse {
		return
	}
	pointer := C.CBytes(raw)
	if pointer == nil {
		return
	}
	response.ptr = pointer
	response.len = C.size_t(len(raw))
}
