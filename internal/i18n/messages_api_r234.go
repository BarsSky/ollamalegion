package i18n

// messages_api_r234.go — R234, батч «api»: логи и ошибки пакета internal/api.
//
// ЗАЧЕМ ОТДЕЛЬНЫМ ФАЙЛОМ. Каталог пополняется батчами по областям кода, чтобы
// параллельные правки не конфликтовали в одном messages.go (см. register.go).
// Здесь собраны сообщения одиннадцати файлов пакета api:
//
//	model_share.go, auth_session.go, auth.go, handlers_backends.go,
//	handlers_model_share.go, handlers_placement.go, handlers_placement_apply.go,
//	handlers_placement_plan.go, handlers_placement_validate.go,
//	handlers_rpc_link_r166.go, handlers_image_profiles.go
//
// ПРАВИЛА ТЕ ЖЕ (messages.go): набор fmt-глаголов в RU и EN обязан совпадать
// (TestCatalogVerbParity), ключ не меняется при правке текста.
//
// ОСОБЕННОСТЬ %w. Ключи api.model_share.source_failed/target_failed/
// source_unreachable содержат только текст ДО причины: сам хвост «: %w»
// дописан в коде вызова (fmt.Errorf), чтобы ошибка-причина оставалась
// разворачиваемой через errors.Is/errors.As — fmt.Sprintf (на котором стоит
// i18n.T) глагол %w не поддерживает.
func init() { Register(messagesAPI) }

var messagesAPI = map[string]map[Lang]string{
	// === model_share.go: перенос модели между бэкендами ==================
	"api.model_share.source_required": {
		RU: "source и model обязательны",
		EN: "source and model are required",
	},
	"api.model_share.source_backend_not_found": {
		RU: "бэкенд-источник %q не найден",
		EN: "source backend %q not found",
	},
	"api.model_share.target_required": {
		RU: "нужен хотя бы один бэкенд-приёмник (кроме источника)",
		EN: "at least one receiver backend is required (other than the source)",
	},
	"api.model_share.target_backend_not_found": {
		RU: "бэкенд %q не найден",
		EN: "backend %q not found",
	},
	"api.model_share.target_kind_mismatch": {
		RU: "тип бэкенда не совпадает: %s vs %s (%s)",
		EN: "backend type mismatch: %s vs %s (%s)",
	},
	"api.model_share.all_targets_rejected": {
		RU: "все приёмники отклонены (нет подходящих бэкендов)",
		EN: "all receivers were rejected (no suitable backends)",
	},
	"api.model_share.kind_unsupported": {
		RU: "бэкенд %q типа %q не поддерживает перенос моделей",
		EN: "backend %q of type %q does not support model transfer",
	},
	"api.model_share.canceled_by_operator": {
		RU: "отменено оператором",
		EN: "canceled by the operator",
	},
	"api.model_share.target_already_has_model": {
		RU: "модель уже есть на этом бэкенде (перезапись не запрошена)",
		EN: "the model already exists on this backend (no overwrite requested)",
	},
	"api.model_share.target_disk_shortfall": {
		RU: "на приёмнике мало места: нужно ~%s, свободно %s (резерв %d МБ)",
		EN: "not enough disk space on the receiver: ~%s needed, %s free (reserve %d MB)",
	},
	"api.model_share.target_disk_shortfall_note": {
		RU: "перенос не начат: не хватает места на диске приёмника",
		EN: "transfer not started: the receiver does not have enough disk space",
	},
	"api.model_share.source_failed": {
		RU: "источник %s не отдал модель",
		EN: "source %s did not return the model",
	},
	"api.model_share.source_http_status": {
		RU: "источник %s вернул HTTP %d: %s",
		EN: "source %s returned HTTP %d: %s",
	},
	"api.model_share.target_failed": {
		RU: "приёмник %s не принял поток",
		EN: "receiver %s did not accept the stream",
	},
	"api.model_share.target_http_status": {
		RU: "приёмник %s вернул HTTP %d: %s",
		EN: "receiver %s returned HTTP %d: %s",
	},
	"api.model_share.source_unreachable": {
		RU: "источник %s недоступен",
		EN: "source %s is unreachable",
	},
	"api.model_share.source_model_missing": {
		RU: "на источнике %s нет модели %q: %s",
		EN: "on source %s there is no model %q: %s",
	},
	"api.model_share.source_size_http_status": {
		RU: "источник %s вернул HTTP %d на запрос размера",
		EN: "source %s returned HTTP %d for the size request",
	},
	"api.model_share.bytes_gib": {
		RU: "%.1f ГБ",
		EN: "%.1f GB",
	},
	"api.model_share.bytes_mib": {
		RU: "%d МБ",
		EN: "%d MB",
	},

	// === auth_session.go: пользователи и сессии ==========================
	"api.auth_session.admin_memory_only": {
		RU: "auth: администратор создан в ПАМЯТИ, но файл users.json не записан — после пересоздания контейнера вход пропадёт",
		EN: "auth: the administrator was created in MEMORY, but users.json was not written — the login will be lost after the container is recreated",
	},
	"api.auth_session.admin_hint_mount_volume": {
		RU: "путь должен быть на смонтированном томе: LB_USERS_FILE=/app/data/users.json + volume balancer_data:/app/data",
		EN: "the path must be on a mounted volume: LB_USERS_FILE=/app/data/users.json + volume balancer_data:/app/data",
	},
	"api.auth_session.admin_created_from_env": {
		RU: "auth: создан администратор из LB_ADMIN_USER/LB_ADMIN_PASSWORD — пароль теперь хранится хешем; переменные можно убрать",
		EN: "auth: administrator created from LB_ADMIN_USER/LB_ADMIN_PASSWORD — the password is now stored hashed; the variables can be removed",
	},
	"api.auth_session.admin_password_apply_failed": {
		RU: "auth: не удалось применить новый пароль администратора из .env",
		EN: "auth: failed to apply the new administrator password from .env",
	},
	"api.auth_session.admin_password_updated": {
		RU: "auth: пароль администратора обновлён из LB_ADMIN_PASSWORD (.env)",
		EN: "auth: the administrator password was updated from LB_ADMIN_PASSWORD (.env)",
	},
	"api.auth_session.admin_create_failed": {
		RU: "auth: не удалось создать администратора из .env",
		EN: "auth: failed to create the administrator from .env",
	},
	"api.auth_session.admin_created_from_env_note": {
		RU: "auth: создан администратор из LB_ADMIN_USER/LB_ADMIN_PASSWORD",
		EN: "auth: administrator created from LB_ADMIN_USER/LB_ADMIN_PASSWORD",
	},
	"api.auth_session.admin_created_others_hint": {
		RU: "если в файле остались другие админы — удалите их через PUT /api/v1/auth/users",
		EN: "if other admins remain in the file — remove them via PUT /api/v1/auth/users",
	},
	"api.auth_session.users_read_failed": {
		RU: "auth: не удалось прочитать users.json",
		EN: "auth: failed to read users.json",
	},
	"api.auth_session.users_parse_failed": {
		RU: "auth: users.json не разобран",
		EN: "auth: users.json could not be parsed",
	},
	"api.auth_session.password_too_short": {
		RU: "пароль короче %d символов",
		EN: "the password is shorter than %d characters",
	},
	"api.auth_session.sessions_read_failed": {
		RU: "auth: файл сессий не прочитан — старт без сохранённых сессий",
		EN: "auth: the sessions file was not read — starting without saved sessions",
	},
	"api.auth_session.sessions_parse_failed": {
		RU: "auth: файл сессий не разобран — старт без сохранённых сессий",
		EN: "auth: the sessions file could not be parsed — starting without saved sessions",
	},
	"api.auth_session.sessions_restored": {
		RU: "auth: сессии восстановлены из файла — вход в WebUI пережил рестарт",
		EN: "auth: sessions restored from the file — the WebUI login survived the restart",
	},
	"api.auth_session.admin_required": {
		RU: "Forbidden: требуются права администратора",
		EN: "Forbidden: administrator rights are required",
	},
	"api.auth_session.session_not_found": {
		RU: "сессия не найдена (или id неоднозначен)",
		EN: "session not found (or the id is ambiguous)",
	},
	"api.auth_session.user_save_failed": {
		RU: "не удалось сохранить пользователя: %v",
		EN: "failed to save the user: %v",
	},
	"api.auth_session.user_delete_failed": {
		RU: "не удалось удалить пользователя: %v",
		EN: "failed to delete the user: %v",
	},
	"api.auth_session.cannot_delete_self": {
		RU: "нельзя удалить самого себя",
		EN: "you cannot delete yourself",
	},

	// === auth.go: middleware =============================================
	"api.auth.admin_required_viewer": {
		RU: "Forbidden: требуются права администратора (роль viewer только читает)",
		EN: "Forbidden: administrator rights are required (the viewer role is read-only)",
	},
	"api.auth.csrf_check_failed": {
		RU: "Forbidden: отсутствует или неверен X-CSRF-Token",
		EN: "Forbidden: X-CSRF-Token is missing or invalid",
	},

	// === handlers_backends.go: роли и раскладка ==========================
	"api.handlers_backends.restore_roles_failed": {
		RU: "restoreBackendRoles: не удалось прочитать роли",
		EN: "restoreBackendRoles: failed to read the roles",
	},
	"api.handlers_backends.restore_roles_done": {
		RU: "restoreBackendRoles: роли бэкендов восстановлены",
		EN: "restoreBackendRoles: backend roles restored",
	},
	"api.handlers_backends.restore_layout_failed": {
		RU: "restoreBackendRoles: раскладка не применена",
		EN: "restoreBackendRoles: the placement was not applied",
	},
	"api.handlers_backends.restore_layout_applied": {
		RU: "restoreBackendRoles: раскладка применена",
		EN: "restoreBackendRoles: the placement was applied",
	},
	"api.handlers_backends.reapply_role_returned": {
		RU: "reapplyBackendRoles: роль возвращена в запись бэкенда",
		EN: "reapplyBackendRoles: the role was returned to the backend record",
	},
	"api.handlers_backends.reapply_layout_failed": {
		RU: "reapplyBackendRoles: раскладка не применена",
		EN: "reapplyBackendRoles: the placement was not applied",
	},
	"api.handlers_backends.persist_roles_failed": {
		RU: "persistBackendRoles: не удалось сохранить роли",
		EN: "persistBackendRoles: failed to save the roles",
	},
	"api.handlers_backends.role_changed": {
		RU: "updateBackendRole: роль бэкенда изменена",
		EN: "updateBackendRole: the backend role was changed",
	},
	"api.handlers_backends.layout_choice_saved": {
		RU: "backendRPCLayout: выбор оператора сохранён",
		EN: "backendRPCLayout: the operator choice was saved",
	},
	"api.handlers_backends.unknown_cppworker_port": {
		RU: "backend %s: неизвестен порт cppworker",
		EN: "backend %s: unknown cppworker port",
	},
	"api.handlers_backends.layout_degraded": {
		RU: "раскладка деградировала: %s",
		EN: "the placement degraded: %s",
	},
	"api.handlers_backends.delete_backend_role_kept": {
		RU: "deleteBackend: узел с ролью оставлен в списке (откреплён агент, не удалён)",
		EN: "deleteBackend: the node with a role was kept in the list (the agent was detached, not deleted)",
	},
	"api.handlers_backends.delete_backend_role_hint": {
		RU: "чтобы удалить запись — сначала снимите роль (PUT /role с пустым значением)",
		EN: "to delete the record — clear the role first (PUT /role with an empty value)",
	},
	"api.handlers_backends.delete_backend_role_kept_message": {
		RU: "Узел с явной ролью оставлен в списке: агент откреплён, роль и раскладка сохранены. Чтобы удалить запись, сначала снимите роль.",
		EN: "The node with an explicit role was kept in the list: the agent is detached, the role and placement are preserved. To delete the record, clear the role first.",
	},

	// === handlers_model_share.go: HTTP-плоскость переноса ================
	"api.handlers_model_share.hint_expected_body": {
		RU: "ожидается {\"source\":\"<backendId>\",\"model\":\"<имя>\",\"targets\":[\"<backendId>\",...],\"overwrite\":false}",
		EN: "expected {\"source\":\"<backendId>\",\"model\":\"<model>\",\"targets\":[\"<backendId>\",...],\"overwrite\":false}",
	},
	"api.handlers_model_share.hint_source_targets": {
		RU: "source — бэкенд, где модель ЕСТЬ; targets — бэкенды того же типа, куда её нужно положить; типы: llama_cpp (файл .gguf) и image_cpp (каталог bundle)",
		EN: "source — the backend that HAS the model; targets — backends of the same type where it must be placed; types: llama_cpp (a .gguf file) and image_cpp (a bundle directory)",
	},
	"api.handlers_model_share.hint_job_list": {
		RU: "GET /api/v1/models/share — список заданий",
		EN: "GET /api/v1/models/share — the list of jobs",
	},
	"api.handlers_model_share.hint_job_lifetime": {
		RU: "задания живут до перезапуска балансера; сам файл либо доехал до приёмника, либо нет — это видно в списке моделей бэкенда",
		EN: "jobs live until the balancer restarts; the file itself either reached the receiver or not — this is visible in the backend model list",
	},
	"api.handlers_model_share.hint_cancel_only": {
		RU: "поддерживается только /cancel",
		EN: "only /cancel is supported",
	},

	// === handlers_placement*.go: политика размещения =====================
	"api.handlers_placement.method_not_allowed": {
		RU: "method not allowed (GET — состояние, PUT — применить политику)",
		EN: "method not allowed (GET — state, PUT — apply the policy)",
	},
	"api.handlers_placement_apply.bad_request_body": {
		RU: "некорректное тело запроса: %v",
		EN: "invalid request body: %v",
	},
	"api.handlers_placement_apply.validation_failed": {
		RU: "политика не применена: конфиг не проходит валидацию (исправьте замечания или подтвердите allowWarnings)",
		EN: "the policy was not applied: the config fails validation (fix the issues or confirm with allowWarnings)",
	},
	"api.handlers_placement_apply.hint_validate": {
		RU: "проверьте тело через POST /api/v1/placement/validate — тот же набор проверок с числами",
		EN: "check the body via POST /api/v1/placement/validate — the same set of checks with numbers",
	},
	"api.handlers_placement_apply.hint_applied": {
		RU: "политика применена к живому балансеру; при persisted=false после рестарта стенд вернётся к конфиг-файлу",
		EN: "the policy is applied to the live balancer; with persisted=false the stand returns to the config file after a restart",
	},
	"api.handlers_placement_apply.config_saver_missing": {
		RU: "configSaver не установлен: конфиг не сохраняется на диск (изменения действуют до перезапуска)",
		EN: "configSaver is not set: the config is not saved to disk (the changes last until a restart)",
	},
	"api.handlers_placement_plan.method_post_only": {
		RU: "method not allowed (только POST: план принимает список моделей)",
		EN: "method not allowed (POST only: the plan accepts a list of models)",
	},
	"api.handlers_placement_plan.bad_request_body": {
		RU: "некорректное тело запроса: %v",
		EN: "invalid request body: %v",
	},
	"api.handlers_placement_validate.method_post_only": {
		RU: "method not allowed (только POST: сухой прогон принимает тело)",
		EN: "method not allowed (POST only: a dry run accepts a body)",
	},
	"api.handlers_placement_validate.bad_request_body": {
		RU: "некорректное тело запроса: %v",
		EN: "invalid request body: %v",
	},
	"api.handlers_placement_validate.group_required": {
		RU: "нужен объект group с составом:",
		EN: "a group object with its members is required:",
	},
	"api.handlers_placement_validate.hint_dry_run": {
		RU: "сухой прогон: ничего не сохранено и не изменено — примените настройку отдельной операцией конфига",
		EN: "dry run: nothing was saved or changed — apply the setting with a separate config operation",
	},

	// === handlers_rpc_link_r166.go: замер канала =========================
	"api.handlers_rpc_link_r166.method_post_only": {
		RU: "method not allowed (только POST)",
		EN: "method not allowed (POST only)",
	},
	"api.handlers_rpc_link_r166.balancer_unavailable": {
		RU: "балансер недоступен",
		EN: "the balancer is unavailable",
	},
	"api.handlers_rpc_link_r166.bad_request_body": {
		RU: "некорректное тело запроса: %v",
		EN: "invalid request body: %v",
	},
	"api.handlers_rpc_link_r166.no_layout_nodes": {
		RU: "нет узлов раскладки: назначьте роли main/donor (без них канал мерить не к чему)",
		EN: "no placement nodes: assign the main/donor roles (there is nothing to measure the link to without them)",
	},
	"api.handlers_rpc_link_r166.measure_requested": {
		RU: "RPC link measure по запросу оператора",
		EN: "RPC link measure requested by the operator",
	},

	// === handlers_image_profiles.go ======================================
	"api.handlers_image_profiles.profile_saved": {
		RU: "профиль %q сохранён; воркер доступен и применит его при загрузке модели",
		EN: "profile %q saved; the worker is reachable and will apply it when the model is loaded",
	},
	"api.handlers_image_profiles.family_required": {
		RU: "family is required (см. types.ImageModelFamilies)",
		EN: "family is required (see types.ImageModelFamilies)",
	},
}
