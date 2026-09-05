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
	Terminate    bool   `json:"Terminate,omitempty"`
	StatusCode   int    `json:"StatusCode,omitempty"`
	ResponseBody []byte `json:"ResponseBody,omitempty"`
	Body         []byte `json:"Body,omitempty"`
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
	fmt.Println("测试 1: 模拟带 max_turns 参数的请求（静态阻断测试）")
	fmt.Println("=======================================================")
	req1 := RequestInterceptRequest{
		RequestID: "req-test-1",
		Body:      []byte(`{"model":"gpt-5.6-luna","max_turns":3,"input":[{"role":"user","content":"hello"}]}`),
	}
	b1, _ := json.Marshal(req1)
	resp1, err := callPlugin("request.intercept_before", b1)
	if err != nil {
		fmt.Printf("测试 1 异常: %v\n", err)
	} else if resp1.Terminate && resp1.StatusCode == 400 {
		fmt.Printf(" [SUCCESS] 测试 1 成功被拦截！\n")
		fmt.Printf("  返回状态码: %d\n", resp1.StatusCode)
		fmt.Printf("  返回 Body: %s\n", string(resp1.ResponseBody))
		fmt.Printf("  是否打上游: 否 (直接在代理层 Short-Circuit)\n")
	} else {
		fmt.Printf(" [FAIL] 测试 1 未按预期拦截: %+v\n", resp1)
	}

	fmt.Println("\n=======================================================")
	fmt.Println("测试 2: 模拟 Responses-Lite 请求（Auto-Fix 智能补齐测试）")
	fmt.Println("=======================================================")
	headers2 := make(http.Header)
	headers2.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	req2 := RequestInterceptRequest{
		RequestID: "req-test-2",
		Headers:   headers2,
		Body:      []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"write a poem"}]}`),
	}
	b2, _ := json.Marshal(req2)
	resp2, err := callPlugin("request.intercept_before", b2)
	if err != nil {
		fmt.Printf("测试 2 异常: %v\n", err)
	} else if !resp2.Terminate && len(resp2.Body) > 0 {
		var fixedBody map[string]any
		_ = json.Unmarshal(resp2.Body, &fixedBody)
		reasoning, _ := fixedBody["reasoning"].(map[string]any)
		if reasoning != nil && reasoning["context"] == "all_turns" {
			fmt.Printf(" [SUCCESS] 测试 2 智能修复成功！\n")
			fmt.Printf("  是否终止: 否 (Terminate = false, 允许放行)\n")
			fmt.Printf("  自动补全的 reasoning 字段: %+v\n", reasoning)
			fmt.Printf("  修改后的完整 Body: %s\n", string(resp2.Body))
		} else {
			fmt.Printf(" [FAIL] reasoning.context 未成功补齐: %s\n", string(resp2.Body))
		}
	} else {
		fmt.Printf(" [FAIL] 测试 2 未按预期修改 Body: %+v\n", resp2)
	}

	fmt.Println("\n=======================================================")
	fmt.Println("测试 3: 模拟同会话 90% 相似度连续报错（会话级 3h 模糊熔断测试）")
	fmt.Println("=======================================================")
	sessionID := "sim-session-circuit-888"
	mockUpstream502 := []byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`)

	inputList := []map[string]string{
		{"role": "user", "content": "task 1"},
		{"role": "assistant", "content": "reply 1"},
		{"role": "user", "content": "task 2"},
		{"role": "assistant", "content": "reply 2"},
		{"role": "user", "content": "task 3"},
		{"role": "assistant", "content": "reply 3"},
		{"role": "user", "content": "task 4"},
		{"role": "assistant", "content": "reply 4"},
		{"role": "user", "content": "task 5"},
		{"role": "assistant", "content": "reply 5"},
	}
	bodyData := map[string]any{
		"model": "gpt-5.6-luna",
		"client_metadata": map[string]any{
			"session_id": sessionID,
		},
		"input": inputList,
	}
	rawBody3, _ := json.Marshal(bodyData)

	fmt.Println("-> 步骤 3.1: 记录第 1 次上游返回 502 报错")
	postReq1 := RequestInterceptRequest{
		RequestID:    "req-post-1",
		StatusCode:   502,
		Body:         rawBody3,
		ResponseBody: mockUpstream502,
	}
	pb1, _ := json.Marshal(postReq1)
	_, _ = callPlugin("request.intercept_after", pb1)

	chk1 := RequestInterceptRequest{RequestID: "chk-1", Body: rawBody3}
	cb1, _ := json.Marshal(chk1)
	chkResp1, _ := callPlugin("request.intercept_before", cb1)
	if chkResp1.Terminate {
		fmt.Println(" [FAIL] 仅错 1 次不应提前阻断！")
	} else {
		fmt.Println("   第 1 次报错后放行正常 (未提前误熔断)")
	}

	fmt.Println("-> 步骤 3.2: 记录第 2 次上游返回 502 报错 (追加 1 条日志，相似度 10/10 = 100% >= 90%)")
	inputList2 := append(inputList, map[string]string{"role": "user", "content": "task 6 retry"})
	bodyData["input"] = inputList2
	rawBody3_2, _ := json.Marshal(bodyData)
	postReq2 := RequestInterceptRequest{
		RequestID:    "req-post-2",
		StatusCode:   502,
		Body:         rawBody3_2,
		ResponseBody: mockUpstream502,
	}
	pb2, _ := json.Marshal(postReq2)
	_, _ = callPlugin("request.intercept_after", pb2)

	fmt.Println("-> 步骤 3.3: 第 3 次请求到来，测试是否触发 3h 熔断直接短路")
	chk2 := RequestInterceptRequest{RequestID: "chk-2", Body: rawBody3_2}
	cb2, _ := json.Marshal(chk2)
	chkResp2, _ := callPlugin("request.intercept_before", cb2)
	if chkResp2 != nil && chkResp2.Terminate && chkResp2.StatusCode == 502 {
		fmt.Printf(" [SUCCESS] 测试 3 智能熔断成功触发！\n")
		fmt.Printf("  拦截状态码: %d\n", chkResp2.StatusCode)
		fmt.Printf("  直接返回上次上游的错误 Body: %s\n", string(chkResp2.ResponseBody))
		fmt.Printf("  是否打上游: 否 (在 3h 冷却期内直接拦截死循环)\n")
	} else {
		fmt.Printf(" [FAIL] 测试 3 未按预期触发熔断: %+v\n", chkResp2)
	}

	fmt.Println("\n-> 步骤 3.4: 换一个全新 session_id 发送相同内容，验证隔离性（不误伤新会话）")
	bodyData["client_metadata"] = map[string]any{"session_id": "new-clean-session-999"}
	rawBodyClean, _ := json.Marshal(bodyData)
	chkClean := RequestInterceptRequest{RequestID: "chk-clean", Body: rawBodyClean}
	cbClean, _ := json.Marshal(chkClean)
	cleanResp, _ := callPlugin("request.intercept_before", cbClean)
	if cleanResp != nil && cleanResp.Terminate {
		fmt.Println(" [FAIL] 新会话被错误误伤！")
	} else {
		fmt.Println(" [SUCCESS] 新会话正常放行，完全无误伤！")
	}
	fmt.Println("=======================================================")
}
