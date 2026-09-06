package types

import (
	"encoding/json"
	"net/http"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 3

	PluginID   = "cpa-codex-guard"
	PluginName = "CPA Codex Guard"
	Version    = "0.2.0"

	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"
	MethodPluginShutdown    = "plugin.shutdown"

	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"
	MethodResponseInterceptAfter = "response.intercept_after"
	MethodRequestComplete        = "request.complete"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	Description      string        `json:"Description"`
	GitHubRepository string        `json:"GitHubRepository"`
	ConfigFields     []ConfigField `json:"ConfigFields,omitempty"`
}

type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type Capabilities struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
}

type LifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type RequestInterceptRequest struct {
	RequestID       string         `json:"RequestID"`
	TraceID         string         `json:"TraceID,omitempty"`
	SourceFormat    string         `json:"SourceFormat"`
	ToFormat        string         `json:"ToFormat,omitempty"`
	Model           string         `json:"Model,omitempty"`
	RequestedModel  string         `json:"RequestedModel,omitempty"`
	Stream          bool           `json:"Stream,omitempty"`
	Headers         http.Header    `json:"Headers,omitempty"`
	RequestHeaders  http.Header    `json:"RequestHeaders,omitempty"`
	Body            []byte         `json:"Body,omitempty"`
	RequestBody     []byte         `json:"RequestBody,omitempty"`
	StatusCode      int            `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header    `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte         `json:"ResponseBody,omitempty"`
	Metadata        map[string]any `json:"Metadata,omitempty"`
}

func (r *RequestInterceptRequest) GetRequestBody() []byte {
	if len(r.RequestBody) > 0 {
		return r.RequestBody
	}
	return r.Body
}

func (r *RequestInterceptRequest) GetResponseBody() []byte {
	if len(r.ResponseBody) > 0 {
		return r.ResponseBody
	}
	return r.Body
}

func (r *RequestInterceptRequest) GetHeaders() http.Header {
	if r.RequestHeaders != nil {
		return r.RequestHeaders
	}
	return r.Headers
}

type RequestInterceptResponse struct {
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	Body            []byte      `json:"Body,omitempty"`
	Headers         http.Header `json:"Headers,omitempty"`
	ClearHeaders    []string    `json:"ClearHeaders,omitempty"`
}

// RequestCompletion mirrors pluginapi.RequestCompletion (schema v3, no json tags:
// fields marshal with Go field names).
type RequestCompletion struct {
	RequestID      string         `json:"RequestID"`
	TraceID        string         `json:"TraceID"`
	SourceFormat   string         `json:"SourceFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	Stream         bool           `json:"Stream"`
	Outcome        string         `json:"Outcome"`
	StatusCode     int            `json:"StatusCode"`
	Error          string         `json:"Error"`
	Metadata       map[string]any `json:"Metadata"`
}
