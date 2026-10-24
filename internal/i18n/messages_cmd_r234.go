package i18n

// R234: сообщения компонентов cmd/* и tools/*.
//
// Здесь живут ключи трёх областей: cppworker (разбор CPPWORKER_RPC_SERVERS,
// защита chat template от cgo SIGSEGV, перенос модели между бэкендами),
// sdworker (перенос image-bundle) и provisioning-хелпера tools/fetch-sdcpp
// (движок sd.cpp и модели с HuggingFace).
//
// Тексты перенесены из кода без правки смысла: RU — исходная формулировка,
// EN — перевод с сохранением технических терминов (backend, worker, model,
// repo, HF, file). Набор fmt-глаголов в ru и en одинаков — это проверяет
// TestCatalogVerbParity.
var messagesCmdR234 = map[string]map[Lang]string{
	// === cppworker: разбор CPPWORKER_RPC_SERVERS (rpc_env_r101.go) =====
	"cppworker.rpc_env.empty_list": {
		RU: "пустой список",
		EN: "empty list",
	},
	"cppworker.rpc_env.too_many": {
		RU: "узлов %d, максимум %d (проверьте, не лишний ли элемент в списке)",
		EN: "%d nodes given, the maximum is %d (check for a stray item in the list)",
	},
	"cppworker.rpc_env.empty_item": {
		RU: "элемент %d пуст (лишняя запятая?)",
		EN: "item %d is empty (stray comma?)",
	},
	"cppworker.rpc_env.bad_hostport": {
		RU: "элемент %d (%q): ожидается host:port — %v",
		EN: "item %d (%q): host:port expected — %v",
	},
	"cppworker.rpc_env.empty_host": {
		RU: "элемент %d (%q): пустой host",
		EN: "item %d (%q): empty host",
	},
	"cppworker.rpc_env.bad_port": {
		RU: "элемент %d (%q): порт не число",
		EN: "item %d (%q): port is not a number",
	},
	"cppworker.rpc_env.port_range": {
		RU: "элемент %d (%q): порт %d вне диапазона 1..65535",
		EN: "item %d (%q): port %d is outside the 1..65535 range",
	},
	"cppworker.rpc_env.duplicate_host": {
		RU: "элемент %d (%q): узел указан дважды",
		EN: "item %d (%q): node listed twice",
	},

	// === cppworker: защита chat template (chat_template_guard.go) ======
	"cppworker.chat_template_guard.disabled": {
		RU: "chat template отключён для модели до перезапуска cppworker: " +
			"cgo SIGSEGV в chat template (повторные вызовы не выполняются, " +
			"промпт собирается вручную)",
		EN: "chat template disabled for the model until cppworker restarts: " +
			"cgo SIGSEGV in chat template (further calls are skipped, " +
			"the prompt is built manually)",
	},
	"cppworker.chat_template_guard.disabled_hint": {
		RU: "проверьте n_ctx/template модели; для полного сброса состояния C перезапустите cppworker",
		EN: "check the model n_ctx/template; to fully reset the C state restart cppworker",
	},
	"cppworker.chat_template_guard.reenabled": {
		RU: "chat template разблокирован для модели (загружена заново)",
		EN: "chat template unblocked for the model (reloaded)",
	},
	"cppworker.chat_template_guard.broken": {
		RU: "chat template отключён после cgo SIGSEGV (%s); промпт собирается вручную",
		EN: "chat template disabled after cgo SIGSEGV (%s); the prompt is built manually",
	},

	// === cppworker: перенос модели (handlers_model_share.go) ===========
	"cppworker.model_share.no_models": {
		RU: "(каталог пуст)",
		EN: "(directory is empty)",
	},
	"cppworker.model_share.hint_list_files": {
		RU: "имя можно посмотреть в GET /api/models/files (или в панели, список моделей бэкенда)",
		EN: "the name is listed by GET /api/models/files (or in the panel, backend model list)",
	},
	"cppworker.model_share.hint_plain_name": {
		RU: "передайте filename=<model>.gguf без каталогов",
		EN: "pass filename=<model>.gguf without directories",
	},
	"cppworker.model_share.hint_gguf_only": {
		RU: "передайте filename=<model>.gguf (каталоги/прочие расширения не переносятся)",
		EN: "pass filename=<model>.gguf (directories and other extensions are not transferred)",
	},
	"cppworker.model_share.hint_overwrite": {
		RU: "повторите с overwrite=1, если файл нужно заменить",
		EN: "repeat with overwrite=1 if the file must be replaced",
	},
	"cppworker.model_share.hint_size_mismatch": {
		RU: "копия прервалась (сеть/диск) — повторите перенос",
		EN: "the copy was interrupted (network/disk) — repeat the transfer",
	},
	"cppworker.model_share.sha256_mismatch": {
		RU: "sha256 mismatch — файл побился при переносе",
		EN: "sha256 mismatch — the file was corrupted during the transfer",
	},

	// === sdworker: перенос image-bundle (handlers_model_share.go) ======
	"sdworker.model_share.bundle_empty": {
		RU: "bundle %q is empty (нет файлов для переноса)",
		EN: "bundle %q is empty (no files to transfer)",
	},
	"sdworker.model_share.extract_failed": {
		RU: "распаковка прервана: %v (получено %d байт, файлов %d)",
		EN: "unpacking interrupted: %v (received %d bytes, %d files)",
	},
	"sdworker.model_share.bytes_mismatch": {
		RU: "перенос неполный: получено %d байт полезной нагрузки, ожидалось %d",
		EN: "transfer incomplete: received %d payload bytes, expected %d",
	},
	"sdworker.model_share.filecount_mismatch": {
		RU: "получено файлов %d, ожидалось %d",
		EN: "received %d files, expected %d",
	},
	"sdworker.model_share.empty_tar": {
		RU: "tar содержит 0 файлов",
		EN: "tar contains 0 files",
	},

	// === tools/fetch-sdcpp: provisioning image-стенда (main.go) ========
	"tools.fetch_sdcpp.error_prefix": {
		RU: "ОШИБКА: %v",
		EN: "ERROR: %v",
	},
	"tools.fetch_sdcpp.usage": {
		RU: `fetch-sdcpp — скачивание движка sd.cpp и моделей

  list-release <owner/repo> [substring]            список ассетов последнего релиза
  get-release  <owner/repo> <substring> <destdir>  скачать и распаковать ассет (zip/tar.gz)
  hf-list      <repo> [substring]                  список файлов репозитория HF
  hf-get       <repo> <filename> <destdir>         скачать файл с HF

Переменные: HF_TOKEN (или HUGGING_FACE_HUB_TOKEN) — токен HuggingFace.
`,
		EN: `fetch-sdcpp — downloads the sd.cpp engine and models

  list-release <owner/repo> [substring]            list the assets of the latest release
  get-release  <owner/repo> <substring> <destdir>  download and unpack an asset (zip/tar.gz)
  hf-list      <repo> [substring]                  list the files of an HF repository
  hf-get       <repo> <filename> <destdir>         download a file from HF

Variables: HF_TOKEN (or HUGGING_FACE_HUB_TOKEN) — HuggingFace token.
`,
	},
	"tools.fetch_sdcpp.owner_repo_required": {
		RU: "нужен <owner/repo>",
		EN: "<owner/repo> is required",
	},
	"tools.fetch_sdcpp.release_header": {
		RU: "релиз %s (%s)",
		EN: "release %s (%s)",
	},
	"tools.fetch_sdcpp.get_release_args_required": {
		RU: "нужны <owner/repo> <substring> <destdir>",
		EN: "<owner/repo> <substring> <destdir> are required",
	},
	"tools.fetch_sdcpp.asset_not_found": {
		RU: "в релизе %s нет ассета с %q (см. list-release)",
		EN: "release %s has no asset matching %q (see list-release)",
	},
	"tools.fetch_sdcpp.unsupported_archive": {
		RU: "распаковка %s не поддерживается (только .zip)",
		EN: "unpacking %s is not supported (only .zip)",
	},
	"tools.fetch_sdcpp.unpacked_file": {
		RU: "распакован %s (%.1f MB)",
		EN: "unpacked %s (%.1f MB)",
	},
	"tools.fetch_sdcpp.repo_required": {
		RU: "нужен <repo>",
		EN: "<repo> is required",
	},
	"tools.fetch_sdcpp.repo_header": {
		RU: "репозиторий %s",
		EN: "repository %s",
	},
	"tools.fetch_sdcpp.hf_get_args_required": {
		RU: "нужны <repo> <filename> <destdir>",
		EN: "<repo> <filename> <destdir> are required",
	},
	"tools.fetch_sdcpp.attempt_failed": {
		RU: "попытка %d/%d не удалась: %v",
		EN: "attempt %d/%d failed: %v",
	},
	"tools.fetch_sdcpp.download_failed": {
		RU: "не удалось скачать после %d попыток",
		EN: "download failed after %d attempts",
	},
	"tools.fetch_sdcpp.stream_read": {
		RU: "чтение потока",
		EN: "reading the stream",
	},
	"tools.fetch_sdcpp.bytes_short": {
		RU: "получено %d из %d байт",
		EN: "received %d of %d bytes",
	},
}

func init() { Register(messagesCmdR234) }
