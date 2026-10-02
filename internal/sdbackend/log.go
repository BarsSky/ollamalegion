package sdbackend

import (
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"

	"go.uber.org/zap"
)

// sdLog — логгер пакета.
//
// Через функцию, а не через поле: logger.Init вызывается из main уже после
// создания супервизора, и «сохранённый» логгер остался бы дефолтным
// (no-op/development). Так же сделано в cppworker (packageLogger).
func sdLog() *zap.SugaredLogger { return logger.Get() }

// seedModeOf — режим seed профиля ("random" по умолчанию).
//
// Нужен для: (1) sidecar-конфига; (2) решения, подставлять ли per-request seed
// в <sd_cpp_extra_args>. R-Image ловушка №1: OpenAI-ветка sd-server НЕ читает
// seed вообще и берёт default_gen_params.seed = 42 — без подстановки все
// картинки одинаковые.
func seedModeOf(p *types.ImageModelProfile) string {
	if p == nil {
		return "random"
	}
	if p.Runtime.SeedMode == "fixed" {
		return "fixed"
	}
	return "random"
}
