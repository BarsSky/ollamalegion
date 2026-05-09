package distinference

// Worker — Go-native worker для распределённого инференса.
// Содержит CGo-модуль (LLama.cpp binding) и gRPC сервер для
// коммуникации с другими Worker'ами и Engine.
type Worker struct {
	id           string
	host         string
	grpcPort     int
	layerRange   string // "1-40"
	gpuMode      string // "auto" | "gpu" | "cpu"
	maxBatchSize int
	kvCacheSizeMB int
}

// NewWorker создаёт нового Worker'а.
func NewWorker(id, host string, grpcPort int, layerRange, gpuMode string) *Worker {
	return &Worker{
		id:         id,
		host:       host,
		grpcPort:   grpcPort,
		layerRange: layerRange,
		gpuMode:    gpuMode,
	}
}

// ID возвращает идентификатор Worker'а.
func (w *Worker) ID() string {
	return w.id
}

// Start запускает Worker (gRPC сервер + CGo).
func (w *Worker) Start() error {
	return nil
}

// Stop останавливает Worker.
func (w *Worker) Stop() error {
	return nil
}

// Infer выполняет инференс на своём наборе слоёв.
func (w *Worker) Infer(input []byte) ([]byte, error) {
	_ = input
	// TODO: Forward to CGo (LLama.cpp) for inference on assigned layers
	return nil, nil
}
