package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ollama-loadbalancer/pkg/types"
)

// TestNewMessage - проверка создания сообщения
func TestNewMessage(t *testing.T) {
	t.Parallel()

	msg, err := NewMessage("test-type", "agent-1", `{"data": "test"}`)
	require.NoError(t, err)

	assert.Equal(t, MessageType("test-type"), msg.Type)
	assert.Equal(t, "agent-1", msg.AgentID)
	assert.NotNil(t, msg.Data)
	assert.False(t, msg.Timestamp.IsZero())
}

// TestNewMessageEmptyType - проверка создания сообщения с пустым типом
func TestNewMessageEmptyType(t *testing.T) {
	t.Parallel()

	// Пустой тип допустим в текущей реализации
	msg, err := NewMessage("", "agent-1", `{"data": "test"}`)
	require.NoError(t, err)
	assert.Equal(t, MessageType(""), msg.Type)
}

// TestMarshal - проверка сериализации
func TestMarshal(t *testing.T) {
	t.Parallel()

	msg, err := NewMessage("metrics", "agent-1", map[string]int{"cpu": 50})
	require.NoError(t, err)

	data, err := msg.Marshal()
	require.NoError(t, err)

	// Проверяем что данные - валидный JSON
	var decoded Message
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, MessageType("metrics"), decoded.Type)
}

// TestUnmarshalMessage - проверка десериализации сообщения
func TestUnmarshalMessage(t *testing.T) {
	t.Parallel()

	msg, err := NewMessage("heartbeat", "agent-1", map[string]string{"status": "ok"})
	require.NoError(t, err)

	data, err := msg.Marshal()
	require.NoError(t, err)

	decoded, err := UnmarshalMessage(data)
	require.NoError(t, err)
	assert.Equal(t, msg.Type, decoded.Type)
	assert.Equal(t, "agent-1", decoded.AgentID)
}

// TestUnmarshalMessageInvalid - проверка десериализации некорректных данных
func TestUnmarshalMessageInvalid(t *testing.T) {
	t.Parallel()

	_, err := UnmarshalMessage([]byte("invalid json"))
	assert.Error(t, err)
}

// TestUnmarshalData - проверка десериализации данных
func TestUnmarshalData(t *testing.T) {
	t.Parallel()

	type TestData struct {
		Status string `json:"status"`
		CPU    int    `json:"cpu"`
	}

	msg, err := NewMessage("metrics", "agent-1", TestData{Status: "ok", CPU: 50})
	require.NoError(t, err)

	data, err := msg.Marshal()
	require.NoError(t, err)

	decoded, err := UnmarshalMessage(data)
	require.NoError(t, err)

	var result TestData
	err = decoded.UnmarshalData(&result)
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Status)
	assert.Equal(t, 50, result.CPU)
}

// TestNewRegisterRequest - проверка создания запроса регистрации
func TestNewRegisterRequest(t *testing.T) {
	t.Parallel()

	req := NewRegisterRequest("agent-1", "localhost", "linux", "amd64", 1, []string{"NVIDIA A100"}, "0.1.0", 11434)

	assert.Equal(t, "agent-1", req.AgentID)
	assert.Equal(t, "localhost", req.Hostname)
	assert.Equal(t, "linux", req.OS)
	assert.Equal(t, "amd64", req.Arch)
	assert.Equal(t, 1, req.GPUCount)
	assert.Equal(t, []string{"NVIDIA A100"}, req.GPUModels)
	assert.Equal(t, "0.1.0", req.OllamaVersion)
	assert.Equal(t, 11434, req.OllamaPort)
}

// TestNewMetricsMessage - проверка создания сообщения метрик
func TestNewMetricsMessage(t *testing.T) {
	t.Parallel()

	metrics := &types.BackendMetrics{
		ID: "agent-1",
		System: types.SystemMetrics{
			CPU: types.CPUMetrics{
				UsagePercent: 50.0,
			},
		},
	}

	msg := NewMetricsMessage("agent-1", 1, metrics)

	assert.Equal(t, "agent-1", msg.AgentID)
	assert.Equal(t, int64(1), msg.Sequence)
	assert.NotNil(t, msg.Metrics)
}

// TestNewHeartbeatMessage - проверка создания heartbeat сообщения
func TestNewHeartbeatMessage(t *testing.T) {
	t.Parallel()

	msg := NewHeartbeatMessage("agent-1", 1, 3600, "healthy")

	assert.Equal(t, "agent-1", msg.AgentID)
	assert.Equal(t, int64(1), msg.Sequence)
	assert.Equal(t, int64(3600), msg.Uptime)
	assert.Equal(t, "healthy", msg.Status)
}