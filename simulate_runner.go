package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>

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

typedef int (*init_fn)(cliproxy_host_api*, cliproxy_plugin_api*);

static void* load_lib(const char* path) {
	return dlopen(path, RTLD_LAZY);
}

static init_fn get_init(void* handle) {
	return (init_fn)dlsym(handle, "cliproxy_plugin_init");
}

static int call_init(init_fn fn, cliproxy_host_api* host, cliproxy_plugin_api* api) {
	return fn(host, api);
}

static int invoke_plugin_call(cliproxy_plugin_call_fn fn, char* method, uint8_t* req, size_t req_len, cliproxy_buffer* buf) {
	return fn(method, req, req_len, buf);
}

static void invoke_plugin_free(cliproxy_plugin_free_fn fn, void* ptr, size_t size) {
	if (fn && ptr) {
		fn(ptr, size);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unsafe"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type RequestInterceptRequest struct {
	RequestID    string         `json:"RequestID"`
	Headers      http.Header    `json:"Headers,omitempty"`
	Body         []byte         `json:"Body,omitempty"`
	StatusCode   int            `json:"StatusCode,omitempty"`
	ResponseBody []byte         `json:"ResponseBody,omitempty"`
	Metadata     map[string]any `json:"Metadata,omitempty"`
}

type RequestInterceptResponse struct {
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	Body            []byte      `json:"Body,omitempty"`
}

func main() {
	soPath := "/home/cwhypt/cliproxyapi/plugins/linux/arm64/cpa-codex-guard-v0.1.0.so"
	cPath := C.CString(soPath)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.load_lib(cPath)
	if handle == nil {
		fmt.Printf("FAILED: dlopen %s: %s\n", soPath, C.GoString(C.dlerror()))
		os.Exit(1)
	}

	initFunc := C.get_init(handle)
	if initFunc == nil {
		fmt.Println("FAILED: dlsym cliproxy_plugin_init")
		os.Exit(1)
	}

	var hostApi C.cliproxy_host_api
	var pluginApi C.cliproxy_plugin_api

	ret := C.call_init(initFunc, &hostApi, &pluginApi)
	if ret != 0 {
		fmt.Printf("FAILED: init returned %d\n", ret)
		os.Exit(1)
	}
	fmt.Printf("[OK] 成功从动态库 %s 加载并初始化插件 (ABI v%d)\n", soPath, pluginApi.abi_version)

	callPlugin := func(method string, payload []byte) (*RequestInterceptResponse, error) {
		cMethod := C.CString(method)
		defer C.free(unsafe.Pointer(cMethod))

		var cBuf C.cliproxy_buffer
		var reqPtr *C.uint8_t
		var reqLen C.size_t
		if len(payload) > 0 {
			reqPtr = (*C.uint8_t)(unsafe.Pointer(&payload[0]))
			reqLen = C.size_t(len(payload))
		}

		res := C.invoke_plugin_call(pluginApi.call, cMethod, reqPtr, reqLen, &cBuf)
		if res != 0 {
			return nil, fmt.Errorf("call error: %d", res)
		}
		defer C.invoke_plugin_free(pluginApi.free_buffer, cBuf.ptr, cBuf.len)

		respBytes := C.GoBytes(cBuf.ptr, C.int(cBuf.len))
		var env Envelope
		if err := json.Unmarshal(respBytes, &env); err != nil {
			return nil, fmt.Errorf("unmarshal envelope: %v (raw: %s)", err, string(respBytes))
		}
		if !env.OK {
			return nil, fmt.Errorf("envelope not ok: %v", env.Error)
		}

		var interceptResp RequestInterceptResponse
		if len(env.Result) > 0 {
			_ = json.Unmarshal(env.Result, &interceptResp)
		}
		return &interceptResp, nil
	}

	fmt.Println("\n=======================================================")
	fmt.Println("测试: 模拟测活短流量样本收集 (3个不同样本) 与随机回放测试")
	fmt.Println("=======================================================")

	probeReqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}`)

	// 1. 模拟上游返回 3 个不同的成功回复（输出 <= 200 字符）
	replies := []string{
		"Pong! System operational.",
		"Hello! OpenAI API is active.",
		"Hi! Health check passed.",
	}

	for i, rText := range replies {
		mockRespJSON := fmt.Sprintf(`{
			"id": "chatcmpl-mock-%d",
			"object": "chat.completion",
			"model": "gpt-4o",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "%s"
				}
			}]
		}`, i+1, rText)

		postReq := RequestInterceptRequest{
			RequestID:    fmt.Sprintf("probe-sample-%d", i+1),
			StatusCode:   200,
			Body:         probeReqBody,
			ResponseBody: []byte(mockRespJSON),
		}
		b, _ := json.Marshal(postReq)
		_, _ = callPlugin("request.intercept_after", b)
		fmt.Printf("-> 成功学习样本 %d: %q\n", i+1, rText)
	}

	fmt.Println("\n-> 模拟第 4 次完全相同的 ping 请求到来（非流式）...")
	chkReq1 := RequestInterceptRequest{
		RequestID: "probe-req-4-nonstream",
		Body:      probeReqBody,
	}
	bChk1, _ := json.Marshal(chkReq1)
	hitResp1, _ := callPlugin("request.intercept_before", bChk1)
	if hitResp1 != nil && hitResp1.Terminate && hitResp1.StatusCode == 200 {
		fmt.Printf(" [SUCCESS] 成功命中测活缓存！(非流式)\n")
		fmt.Printf("  状态码: %d\n", hitResp1.StatusCode)
		fmt.Printf("  返回 Body: %s\n", string(hitResp1.ResponseBody))
	} else {
		fmt.Printf(" [FAIL] 测活缓存未命中: %+v\n", hitResp1)
	}

	fmt.Println("\n-> 模拟第 5 次完全相同的 ping 请求到来（流式 stream: true）...")
	probeStreamReqBody := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	chkReq2 := RequestInterceptRequest{
		RequestID: "probe-req-5-stream",
		Body:      probeStreamReqBody,
	}
	bChk2, _ := json.Marshal(chkReq2)
	hitResp2, _ := callPlugin("request.intercept_before", bChk2)
	if hitResp2 != nil && hitResp2.Terminate && hitResp2.StatusCode == 200 {
		fmt.Printf(" [SUCCESS] 成功命中测活缓存！(流式 SSE 自动转码)\n")
		fmt.Printf("  Content-Type: %s\n", hitResp2.ResponseHeaders.Get("Content-Type"))
		fmt.Printf("  返回 SSE 片段:\n%s\n", string(hitResp2.ResponseBody))
	} else {
		fmt.Printf(" [FAIL] 测活流式缓存未命中: %+v\n", hitResp2)
	}

	fmt.Println("-> 多次调用测试随机性（验证样本不是固定单一返回）:")
	seenResponses := make(map[string]int)
	for i := 0; i < 10; i++ {
		chk := RequestInterceptRequest{RequestID: fmt.Sprintf("rand-%d", i), Body: probeReqBody}
		b, _ := json.Marshal(chk)
		res, _ := callPlugin("request.intercept_before", b)
		if res != nil {
			for _, r := range replies {
				if strings.Contains(string(res.ResponseBody), r) {
					seenResponses[r]++
				}
			}
		}
	}
	for k, count := range seenResponses {
		fmt.Printf("   样本 %-30s 随机命中次数: %d/10\n", fmt.Sprintf("%q", k), count)
	}
	if len(seenResponses) > 1 {
		fmt.Println(" [SUCCESS] 验证通过：回复呈现真实的非固定随机分布！")
	}
	fmt.Println("=======================================================")
}
