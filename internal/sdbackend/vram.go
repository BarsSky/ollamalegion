package sdbackend

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================
// VRAM через nvidia-smi (best-effort)
// ============================================================
//
// ЗАЧЕМ: план (§5.6) требует «VRAM если доступно». Обязательным это не
// является: воркер должен работать и на AMD/Intel (Vulkan), где nvidia-smi нет
// вовсе, — поэтому все ошибки глушим и отдаём Available=false.
//
// Кэш на 10 с: nvidia-smi — внешний процесс, его нельзя дёргать на каждый
// scrape Prometheus (иначе метрики дороже самой генерации).

// VRAMInfo — снимок памяти GPU.
type VRAMInfo struct {
	Available bool   `json:"available"`
	UsedMB    int64  `json:"used_mb,omitempty"`
	TotalMB   int64  `json:"total_mb,omitempty"`
	Source    string `json:"source,omitempty"`
	Error     string `json:"error,omitempty"`
}

// vramCache — кэш последнего успешного чтения.
type vramCache struct {
	mu      sync.Mutex
	at      time.Time
	info    VRAMInfo
	ttl     time.Duration
	binPath string
}

var globalVRAMCache = &vramCache{ttl: 10 * time.Second}

// QueryVRAM читает занятую/полную память GPU (первой в списке nvidia-smi).
func QueryVRAM(ctx context.Context) VRAMInfo {
	if globalVRAMCache == nil {
		return VRAMInfo{}
	}
	return globalVRAMCache.query(ctx)
}

func (c *vramCache) query(ctx context.Context) VRAMInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < c.ttl && (c.info.Available || c.info.Error != "") {
		return c.info
	}

	bin := c.binPath
	if bin == "" {
		p, err := exec.LookPath("nvidia-smi")
		if err != nil {
			c.at, c.info = time.Now(), VRAMInfo{Error: "nvidia-smi not found"}
			return c.info
		}
		bin = p
		c.binPath = p
	}

	// Отдельный таймаут: nvidia-smi на «залипшем» драйвере может висеть
	// десятками секунд, а /metrics не должен из-за этого таймаутить.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin,
		"--query-gpu=memory.used,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		c.at, c.info = time.Now(), VRAMInfo{Error: "nvidia-smi failed: " + err.Error()}
		return c.info
	}

	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	parts := strings.Split(line, ",")
	if len(parts) < 2 {
		c.at, c.info = time.Now(), VRAMInfo{Error: "unexpected nvidia-smi output"}
		return c.info
	}
	used, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	total, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil {
		c.at, c.info = time.Now(), VRAMInfo{Error: "unparsable nvidia-smi output"}
		return c.info
	}
	c.at = time.Now()
	c.info = VRAMInfo{Available: true, UsedMB: used, TotalMB: total, Source: "nvidia-smi"}
	return c.info
}
