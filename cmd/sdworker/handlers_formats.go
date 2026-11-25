// handlers_formats.go — R755 (2026-11-25): GET /api/image/formats — матрица
// форматов «что движок читает» для семейства.
//
// КОНТРАКТ (его читает WebUI: image-models-hf.js — бейджи в списке файлов HF,
// image-model-files.js — бейджи в панели файлов группы):
//
//	GET /api/image/formats?family=qwen_image
//	{
//	  engine, family, familyKnown, dit, allInOne,
//	  rules: [{format, role, verdict}],   // reads|needs_gguf|warn|unsupported|unknown
//	  nameWarnings: [{contains, verdict, code}]
//	}
//
// БЕЗ family — консервативная матрица (safetensors-диффузия = warn): без
// семейства нельзя отличить all-in-one SDXL от DiT-диффузии, и обещать чтение
// нельзя. Решение о конкретном файле всё равно принимает probe (заголовок).
package main

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/imagemodel"
)

// handleImageFormats — отдать матрицу форматов.
func (a *App) handleImageFormats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	family := strings.TrimSpace(r.URL.Query().Get("family"))
	writeJSON(w, http.StatusOK, imagemodel.BuildFormatMatrix(family))
}
