package protocol

import (
	"encoding/json"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// MessageType - тип сообщения протокола
type MessageType string

const (
	MsgTypeRegister      MessageType = "register"
	MsgTypeMetrics       MessageType = "metrics"
	MsgTypeHeartbeat     MessageType = "heartbeat"
	MsgTypeConfigRequest MessageType = "config_request"
	MsgTypeConfigResponse MessageType = "config_response"
	MsgTypeHealthCheck   MessageType = "health_check"
	MsgTypeHealthResponse MessageType = "health_response"
	MsgTypeError         MessageType = "error"
	MsgTypeAck           MessageType = "ack"
)

// Message - базовое сообщение протокола
type Message struct {
	Type      MessageType    `json:"type"`
	AgentID   string         `json:"agentId"`
	Timestamp time.Time      `json:"timestamp"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// RegisterRequest - запрос регистрации агента
type RegisterRequest struct {
	AgentID       string `json:"agentId"`
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	GPUCount      int    `json:"gpuCount"`
	GPUModels     []string `json:"gpuModels"`
	OllamaVersion string `json:"ollamaVersion"`
	OllamaPort    int    `json:"ollamaPort"`
}

// RegisterResponse - ответ на регистрацию
type RegisterResponse struct {
	Success       bool              `json:"success"`
	AgentID       string            `json:"agentId"`
	Config        *types.AgentConfig `json:"config,omitempty"`
	Error         string            `json:"error,omitempty"`
}

// MetricsMessage - сообщение с метриками
type MetricsMessage struct {
	AgentID   string                `json:"agentId"`
	Timestamp time.Time             `json:"timestamp"`
	Sequence  int64                 `json:"sequence"`
	Metrics   *types.BackendMetrics `json:"metrics"`
}

// HeartbeatMessage - heartbeat сообщение
type HeartbeatMessage struct {
	AgentID   string    `json:"agentId"`
	Timestamp time.Time `json:"timestamp"`
	Uptime    int64     `json:"uptime"` // секунды аптайма
	Sequence  int64     `json:"sequence"`
	Status    string    `json:"status"` // healthy, degraded, unhealthy
}

// ConfigRequest - запрос конфигурации от агента
type ConfigRequest struct {
	AgentID string `json:"agentId"`
}

// ConfigResponse - ответ с конфигурацией
type ConfigResponse struct {
	AgentID string             `json:"agentId"`
	Config  *types.AgentConfig `json:"config"`
}

// HealthCheckRequest - запрос проверки здоровья
type HealthCheckRequest struct {
	AgentID   string    `json:"agentId"`
	Timestamp time.Time `json:"timestamp"`
}

// HealthCheckResponse - ответ проверки здоровья
type HealthCheckResponse struct {
	AgentID   string    `json:"agentId"`
	Healthy   bool      `json:"healthy"`
	Latency   int64     `json:"latency"` // ms
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

// ErrorMessage - сообщение об ошибке
type ErrorMessage struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	AgentID   string    `json:"agentId,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// AckMessage - подтверждение получения
type AckMessage struct {
	AgentID   string    `json:"agentId"`
	MessageID string    `json:"messageId"`
	Timestamp time.Time `json:"timestamp"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
}

// NewMessage - создание нового сообщения
func NewMessage(msgType MessageType, agentID string, data interface{}) (*Message, error) {
	var rawData json.RawMessage
	var err error
	
	if data != nil {
		rawData, err = json.Marshal(data)
		if err != nil {
			return nil, err
		}
	}
	
	return &Message{
		Type:      msgType,
		AgentID:   agentID,
		Timestamp: time.Now().UTC(),
		Data:      rawData,
	}, nil
}

// Marshal - сериализация сообщения в JSON
func (m *Message) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// UnmarshalMessage - десериализация сообщения из JSON
func UnmarshalMessage(data []byte) (*Message, error) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// UnmarshalData - десериализация данных сообщения в указанную структуру
func (m *Message) UnmarshalData(v interface{}) error {
	if m.Data == nil {
		return nil
	}
	return json.Unmarshal(m.Data, v)
}

// NewRegisterRequest - создание запроса регистрации
func NewRegisterRequest(agentID, hostname, os, arch string, gpuCount int, gpuModels []string, ollamaVersion string, ollamaPort int) *RegisterRequest {
	return &RegisterRequest{
		AgentID:       agentID,
		Hostname:      hostname,
		OS:            os,
		Arch:          arch,
		GPUCount:      gpuCount,
		GPUModels:     gpuModels,
		OllamaVersion: ollamaVersion,
		OllamaPort:    ollamaPort,
	}
}

// NewMetricsMessage - создание сообщения с метриками
func NewMetricsMessage(agentID string, sequence int64, metrics *types.BackendMetrics) *MetricsMessage {
	return &MetricsMessage{
		AgentID:   agentID,
		Timestamp: time.Now().UTC(),
		Sequence:  sequence,
		Metrics:   metrics,
	}
}

// NewHeartbeatMessage - создание heartbeat сообщения
func NewHeartbeatMessage(agentID string, sequence int64, uptime int64, status string) *HeartbeatMessage {
	return &HeartbeatMessage{
		AgentID:   agentID,
		Timestamp: time.Now().UTC(),
		Uptime:    uptime,
		Sequence:  sequence,
		Status:    status,
	}
}
