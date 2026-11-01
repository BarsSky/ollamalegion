// update_config_keys_r374.go — R374 (2026-11-01): «известные ключи» PUT
// /api/v1/cppworker/config/update не должны отставать от того, что обработчик
// реально применяет.
//
// ЖИВОЙ ФАКТ (двухузловой стенд). Балансер при смене роли донора присылает
// `rpcServerEnabled` и `rpcServerPort`. Обработчик их ПРИМЕНЯЕТ (applyBool /
// applyInt в handlers_config.go), узел реально поднимается и гасится — и тут же
// пишет в лог предупреждение:
//
//	{"level":"warn","msg":"handleCppWorkerUpdateConfig: unknown field",
//	 "field":"rpcServerEnabled","value":true}
//	{"level":"warn",...,"field":"rpcServerPort","value":50052}
//
// То есть warning ровно о том, что только что успешно применено. Оператор видит
// «неизвестное поле» там, где всё работает, и идёт искать несуществующую ошибку.
//
// ПРИЧИНА — ДВА СПИСКА ОБ ОДНОМ. Набор ключей живёт отдельной картой рядом с
// вызовами apply*(); новую настройку добавили в применение, но не в карту (или
// наоборот). Класс уже знаком проекту: так же расходились «две правды» про
// rpcEnabled и про раскладку.
//
// ПОЭТОМУ: карта вынесена сюда как единственный источник, а стюард-тест
// (update_config_keys_r374_test.go) разбирает исходник обработчика и требует,
// чтобы КАЖДЫЙ применяемый ключ был в карте. Новая настройка без записи в карту
// теперь ломает тест, а не тихо печатает «unknown field».
package main

// updateConfigKnownKeysR374 — ключи, которые PUT /api/v1/cppworker/config/update
// принимает и обрабатывает. Всё, чего здесь нет, считается незнакомым и
// логируется предупреждением (это полезный сигнал для WebUI — но только если
// список полный).
var updateConfigKnownKeysR374 = map[string]bool{
	// Базовые
	"defaultCtxSize": true, "defaultBatchSize": true,
	"defaultGpuLayers": true, "defaultFlashAttnType": true,
	"defaultNuma": true, "defaultUseMmap": true, "defaultUseMlock": true,
	"defaultNThreads": true, "defaultNParallel": true, "defaultRmsNormEps": true,
	// Multi-GPU
	"autoGpuDistribution": true, "tensorSplitStrategy": true,
	"defaultMainGpu": true, "defaultRpcBackend": true,
	"defaultNoMemoryMap": true, "defaultTensorSplit": true, "defaultSplitMode": true,
	// KV cache
	"defaultKvCacheType": true, "defaultNoKvOffload": true,
	// RoPE
	"defaultRopeFreqBase": true, "defaultRopeFreqScale": true,
	"defaultRopeScalingType": true, "defaultRopeScalingFactor": true,
	// YaRN
	"defaultYarnExtFactor": true, "defaultYarnAttnFactor": true,
	"defaultYarnBetaFast": true, "defaultYarnBetaSlow": true,
	// Метрики и lifecycle
	"enableMetrics": true, "metricsRetentionSeconds": true, "idleUnloadMinutes": true,
	// R304: роль бэкенда (донор не поднимает своих копий моделей).
	"backendRole": true,
	// Session 18 (Round 11/14): Reasoning/Thinking.
	// enableReasoning/reasoningBudget — runtime-политика (промпт);
	// defaultEnableReasoning/defaultReasoningBudget — то, что наследует
	// НОВАЯ загрузка (R106b, 2026-10-12). Оба набора — не load-affecting.
	"enableReasoning": true, "reasoningBudget": true,
	"defaultEnableReasoning": true, "defaultReasoningBudget": true,
	// R374 (2026-11-01): собственный узел раскладки этой машины. Балансер шлёт
	// эти два ключа при смене роли донора — обработчик их применяет, но карта
	// про них не знала и печатала «unknown field» ровно о применённом.
	"rpcServerEnabled": true, "rpcServerPort": true,
}
