package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
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

static int call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t len, cliproxy_buffer* response) {
    return api->call(api->host_ctx, method, request, len, response);
}
static void free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
    api->free_buffer(ptr, len);
}
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"math"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName  = "codearts"
	abiVersion    = pluginabi.ABIVersion
	schemaVersion = pluginabi.SchemaVersion
)

// version 由构建注入：-ldflags "-X main.version=$(cat VERSION)"。
// 未注入时回退成开发版标记，面板与注册 metadata 都读这个值。
var version = "0.0.0-dev"

var hostAPI *C.cliproxy_host_api

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || host.call == nil || plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	if method == nil || requestLen > C.size_t(math.MaxInt32) || (requestLen != 0 && request == nil) {
		writeResponse(response, errorEnvelope("invalid_request", "invalid method or request buffer"))
		return 1
	}
	var raw []byte
	if requestLen != 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := handleMethod(C.GoString(method), raw)
	if err != nil {
		writeResponse(response, rpcErrorEnvelope(err))
		return 1
	}
	writeResponse(response, result)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {} // The host unloads this c-shared runtime during shutdown.

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	name := C.CString(method)
	defer C.free(unsafe.Pointer(name))
	var input unsafe.Pointer
	if len(request) != 0 {
		input = C.CBytes(request)
		defer C.free(input)
	}
	var response C.cliproxy_buffer
	status := C.call_host(hostAPI, name, (*C.uint8_t)(input), C.size_t(len(request)), &response)
	var out []byte
	if response.ptr != nil {
		if response.len > C.size_t(math.MaxInt32) {
			if hostAPI.free_buffer != nil {
				C.free_host_buffer(hostAPI, response.ptr, response.len)
			}
			return nil, fmt.Errorf("host response too large")
		}
		out = C.GoBytes(response.ptr, C.int(response.len))
		if hostAPI.free_buffer != nil {
			C.free_host_buffer(hostAPI, response.ptr, response.len)
		}
	}
	if status != 0 {
		return nil, fmt.Errorf("host call %s returned %d", method, status)
	}
	return out, nil
}

func okEnvelope(v any) []byte {
	raw, err := json.Marshal(struct {
		OK     bool `json:"ok"`
		Result any  `json:"result"`
	}{true, v})
	if err != nil {
		return errorEnvelope("encode_error", err.Error())
	}
	return raw
}

func errorEnvelope(code, msg string) []byte {
	raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
	return raw
}

func hostUnwrap(raw []byte) (json.RawMessage, error) {
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("host error %s: %s", env.Error.Code, env.Error.Message)
	}
	return env.Result, nil
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		// 宿主只发 config_yaml + schema_version，不提供数据目录，账本位置由插件
		// 自己决定（见 ledgerStorageDir）。
		initLedgerStorage()
		return okEnvelope(map[string]any{
			"schema_version": schemaVersion,
			"metadata":       pluginapi.Metadata{Name: "CodeArts", Version: version, Author: "codearts2api", GitHubRepository: "https://github.com/HITZY2002/codearts2api"},
			"capabilities": map[string]any{
				"model_provider": true, "auth_provider": true, "frontend_auth_provider": false,
				"executor": true, "executor_model_scope": "oauth",
				"executor_input_formats":  []string{"chat-completions"},
				"executor_output_formats": []string{"chat-completions"},
				"management_api":          true, "scheduler": false, "usage_plugin": false,
			},
		}), nil
	case "usage.handle":
		return handleUsageRecord(raw), nil
	case "model.static":
		return handleModelStatic(raw)
	case "model.for_auth":
		return handleModelForAuth(raw)
	case "auth.identifier", "executor.identifier":
		return okEnvelope(map[string]string{"identifier": providerName}), nil
	case "auth.parse":
		return handleParseAuth(raw)
	case "auth.login_start", "auth.login.start":
		return handleLoginStart(raw)
	case "auth.login_poll", "auth.login.poll":
		return handleLoginPoll(raw)
	case "auth.refresh":
		return handleRefreshAuth(raw)
	case "executor.execute":
		return handleExecute(raw)
	case "executor.execute_stream":
		return handleExecuteStream(raw)
	case "executor.count_tokens":
		return okEnvelope(map[string]any{"payload": []byte(`{"input_tokens":0}`)}), nil
	case "management.register":
		return managementRegister(raw)
	case "management.handle":
		return managementHandle(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
