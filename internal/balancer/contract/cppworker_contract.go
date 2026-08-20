// Package contract — wire-format contract tests between balancer and cppworker.
//
// Round 51.5 (2026-08-20): created after R51.4 surfaced that the project had
// NO systematic way to catch payload field drift between producer (balancer)
// and consumer (cppworker). R34 follow-up → R51.4 = 6+ rounds where reload
// silently returned HTTP 400 "unknown field 'reason'" / 'adaptiveStage' for
// all preflight reloads. Cline hung on keepalive because the request never
// reached the backend properly.
//
// 3-layer defense:
//
//   L1 (mirror struct tests, cppworker_contract_test.go): table-driven tests
//       with local Go structs mirroring cppworker's request types. Caught
//       R51.4 in 1ms, no live backend needed.
//
//   L2 (live integration, cppworker_contract_live_test.go, build tag
//       `integration`): against a REAL cppworker. Validates that cppworker
//       ACTUALLY accepts the wire bytes (vs. the L1 mirror that may drift).
//       Distinguishes WIRE errors (400 with "invalid JSON" / "unknown field")
//       from STATE errors (404 model not loaded, etc).
//
//   L3 (R52+, separate round): move mirror structs to shared pkg/types
//       imported by BOTH balancer and cppworker — drift becomes impossible.

package contract

import "time"

// ============================================================
// Mirror structs of cmd/cppworker/types.go
// ============================================================
//
// CRITICAL: mirror structs MUST match cppworker's request types exactly
// (same field names, same JSON tags, same omitempty). When cppworker
// changes, update here in the same commit. The tests in
// cppworker_contract_test.go use json.Decoder + DisallowUnknownFields —
// if these drift, tests fail with a clear message.
//
// SCOPE: only fields BALANCER actually sends. We do NOT mirror every
// possible field — that would over-couple and create noise. We mirror
// what production code (model_management.go, nctx_reload.go, etc.) sends.

// CppworkerReloadRequest — mirror of cmd/cppworker/types.go:134 (reloadModelRequest).
// Fields sent by nctx_reload.go:758 + enrichReloadPayload.
// R51.4: NO "reason" (cppworker struct doesn't have it), NO "adaptiveStage"
// (balancer-internal metadata).
type CppworkerReloadRequest struct {
	Name                string   `json:"name"`
	ContextSize         *int     `json:"contextSize,omitempty"`
	GPULayers           *int     `json:"gpuLayers,omitempty"`
	FlashAttn           *int     `json:"flashAttn,omitempty"`
	UseMmap             *bool    `json:"useMmap,omitempty"`
	Force               *bool    `json:"force,omitempty"`
	KVCacheType         *string  `json:"kvCacheType,omitempty"`
	Parallel            *int     `json:"parallel,omitempty"`
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
}

// CppworkerLoadRequest — mirror of cmd/cppworker/types.go:89 (loadModelRequest).
// Fields sent by nctx_reload_handlers.go:868.
// R44.1: HAS "reason" field. R51.4 confirmed this struct differs from
// reloadModelRequest in this respect.
type CppworkerLoadRequest struct {
	Name                string    `json:"name"`
	ContextSize         *int      `json:"contextSize,omitempty"`
	GPULayers           *int      `json:"gpuLayers,omitempty"`
	KVCacheType         *string   `json:"kvCacheType,omitempty"`
	UseMmap             *bool     `json:"useMmap,omitempty"`
	OverrideTensors     []string  `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string  `json:"overrideTensorBufts,omitempty"`
	Reason              *string   `json:"reason,omitempty"`
}

// CppworkerLoadWithParamsRequest — mirror of cmd/cppworker/types.go:171.
// Fields sent by model_management.go:779 (executeLlamaCppLoad with
// useLoadWithParams=true for MoE override-tensors routing).
type CppworkerLoadWithParamsRequest struct {
	Name                string   `json:"name"`
	ContextSize         *int     `json:"contextSize,omitempty"`
	GPULayers           *int     `json:"gpuLayers,omitempty"`
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
}

// CppworkerUnloadRequest — UNLOAD uses ?name= query parameter, NOT body.
// So no struct needed. We only validate that the path takes a query param.

// CppworkerDeleteModelRequest — for /api/models/delete (POST with body).
// Currently NOT used by balancer (use /api/delete alias via Ollama route).
// Listed for completeness.
type CppworkerDeleteModelRequest struct {
	Name string `json:"name"`
}

// ============================================================
// Response mirrors (for live integration test)
// ============================================================

// CppworkerInfoResponse — minimal fields from /api/info response.
type CppworkerInfoResponse struct {
	Version         string                 `json:"version"`
	Build           string                 `json:"build"`
	LoadedModels    []CppworkerLoadedModel `json:"loaded_models"`
	ReloadPending   map[string]interface{} `json:"reload_pending,omitempty"`
	MaxContextSize  int                    `json:"max_context_size,omitempty"`
	MaxVramNCtx     int                    `json:"max_vram_n_ctx,omitempty"`
	BuildCommit     string                 `json:"build_commit,omitempty"`
	BuildType       string                 `json:"build_type,omitempty"`
}

type CppworkerLoadedModel struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	ContextLength int    `json:"context_length"`
	ContextSize   int    `json:"context_size,omitempty"`
}

// ============================================================
// Endpoint registry
// ============================================================

// Endpoint — single cppworker HTTP endpoint that balancer calls.
type Endpoint struct {
	Name           string
	Method         string
	Path           string
	RequestStruct  interface{} // mirror struct (nil for query-param / passthrough)
	PayloadBuilder func() map[string]interface{}
	Description    string
}

// AllEndpoints — registry of every cppworker endpoint balancer uses.
// NOTE: only endpoints balancer CALLS. cppworker-only endpoints (eg
// /api/pull) excluded.
var AllEndpoints = []Endpoint{
	{
		Name:           "reload",
		Method:         "POST",
		Path:           "/api/models/reload",
		RequestStruct:  &CppworkerReloadRequest{},
		PayloadBuilder: buildReloadPayload,
		Description:    "Reload model with new n_ctx/gpu_layers. R51.4 fix: no reason/adaptiveStage.",
	},
	{
		Name:           "load",
		Method:         "POST",
		Path:           "/api/models/load",
		RequestStruct:  &CppworkerLoadRequest{},
		PayloadBuilder: buildLoadPayload,
		Description:    "Load model. HAS 'reason' (R44.1).",
	},
	{
		Name:           "load-with-params",
		Method:         "POST",
		Path:           "/api/models/load-with-params",
		RequestStruct:  &CppworkerLoadWithParamsRequest{},
		PayloadBuilder: buildLoadWithParamsPayload,
		Description:    "Load with MoE override-tensors (Round 7).",
	},
	{
		Name:        "unload",
		Method:      "POST",
		Path:        "/api/models/unload",
		RequestStruct: nil, // name is a query param, not body
		PayloadBuilder: func() map[string]interface{} { return nil },
		Description:    "Unload model. name=?name=... in query string.",
	},
	{
		Name:           "delete",
		Method:         "POST",
		Path:           "/api/models/delete",
		RequestStruct:  &CppworkerDeleteModelRequest{},
		PayloadBuilder: func() map[string]interface{} { return map[string]interface{}{"name": "gemma-4-E4B-it-Q4_K_M"} },
		Description:    "Delete model from disk (not used by balancer directly).",
	},
	{
		Name:          "info",
		Method:        "GET",
		Path:          "/api/info",
		RequestStruct: nil,
		PayloadBuilder: func() map[string]interface{} { return nil },
		Description:   "Heartbeat / version / loaded models.",
	},
	{
		Name:          "models",
		Method:        "GET",
		Path:          "/api/models",
		RequestStruct: nil,
		PayloadBuilder: func() map[string]interface{} { return nil },
		Description:   "List loaded models.",
	},
	{
		Name:          "version",
		Method:        "GET",
		Path:          "/api/version",
		RequestStruct: nil,
		PayloadBuilder: func() map[string]interface{} { return nil },
		Description:   "cppworker version info.",
	},
	// Proxy passthrough endpoints — balancer doesn't construct payload,
	// but proxyRequest must NOT mutate the body. Covered separately by
	// TestProxyPassthrough_NoMutation in proxy_request_test.go.
}

// buildReloadPayload — production payload из nctx_reload.go:758 + enrichReloadPayload.
func buildReloadPayload() map[string]interface{} {
	return map[string]interface{}{
		"name":         "gemma-4-E4B-it-Q4_K_M",
		"contextSize":  ptrInt(65536),
		"force":        ptrBool(true),
		"gpuLayers":    ptrInt(-2),
		"flashAttn":    ptrInt(-1),
		"useMmap":      ptrBool(true),
		"kvCacheType":  ptrStr("q4_0"),
		"parallel":     ptrInt(1),
		"overrideTensors":     []string{"blk\\.ffn_.*_exps\\.weight"},
		"overrideTensorBufts": []string{"CPU"},
	}
}

// buildLoadPayload — production payload из nctx_reload_handlers.go:868.
// (loadModelRequest HAS reason per R44.1.)
func buildLoadPayload() map[string]interface{} {
	return map[string]interface{}{
		"name":        "gemma-4-E4B-it-Q4_K_M",
		"contextSize": ptrInt(65536),
		"reason":      ptrStr("balancer preflight async auto-load-or-reload (loaded<requested or cppworker restart)"),
		"kvCacheType": ptrStr("q4_0"),
		"gpuLayers":   ptrInt(-2),
		"useMmap":     ptrBool(true),
	}
}

// buildLoadWithParamsPayload — production payload из model_management.go:779.
// Только то что balancer реально шлёт для MoE override-tensors.
func buildLoadWithParamsPayload() map[string]interface{} {
	return map[string]interface{}{
		"name":                "gemma-4-E4B-it-Q4_K_M",
		"contextSize":         ptrInt(32768),
		"gpuLayers":           ptrInt(-2),
		"overrideTensors":     []string{"blk\\.ffn_.*_exps\\.weight"},
		"overrideTensorBufts": []string{"CPU"},
	}
}

// BuildTime — sentinel для drift detection.
var BuildTime = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

// Pointer helpers (вместо `ptr` package).
func ptrInt(v int) *int       { return &v }
func ptrBool(v bool) *bool    { return &v }
func ptrStr(v string) *string { return &v }
