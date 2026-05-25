// Package types содержит все типы данных, используемые в OllamaLegion.
// Типы разнесены по отдельным файлам для удобства навигации:
//
//	backend.go        — Backend, BackendStatus, PlatformMode, OllamaDesiredConfig, HealthCheckResult
//	backend_type.go   — BackendType, BackendEngine, LlamaCppConfig, ModeBackendTypes, ModeEngines
//	model_state.go    — ModelState, WarmupState, ModelDetails
//	metrics.go        — BackendMetrics, GPUMetrics, CPUMetrics, SystemMetrics
//	ollama_metrics.go — OllamaMetrics, OllamaRuntimeFlags, ModelContextInfo, AvailableModel, BackendCapacity, RunningModel
//	llama_cpp_metrics.go — LlamaCppMetrics, LlamaCppModel, LlamaCppGPUInfo, LlamaCppGPUDevice
//	cluster_state.go  — ClusterState, RecentClient
//	prediction.go     — Prediction, MetricsSnapshot
//	session.go        — Session, QueuedRequest, AgentConfig
//	balancing.go      — BalancingAlgorithm, BalancingSettings + все саб-конфиги
//	config.go         — LoadBalancerConfig, LoadBalancerSettings, TLSConfig, AuthConfig, APISettings, LoggingSettings, ResourceLimits
//	rpc_variants.go   — ModelReplicationConfig, RpcCoordinatorConfig, VirtualModelsConfig, DistInferenceConfig
//	event.go          — EventType, Event
package types