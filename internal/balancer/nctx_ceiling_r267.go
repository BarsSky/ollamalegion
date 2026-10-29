package balancer

import "strconv"

// nctx_ceiling_r267.go — R267 (2026-10-29): ОДИН потолок окна контекста для
// всех решений, и видимая причина ограничения.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (после R255). В WebUI выставлено окно 256000, модель
// обучена на 262144, VRAM хватает — а запрос агента на окно больше env-потолка
// по-прежнему получал отказ. Причина оказалась археологической: потолок
// применялся в ДВУХ слоях, и правка одного не лечила другой.
//
//	обработчик preflight (nctx_reload_handlers.go) — уже поднимал потолок до
//	    заявленного окна через effectiveAutoReloadCeiling (R255);
//	РЕШАТЕЛЬ о перезагрузке (nctx_reload.go) — продолжал сравнивать с сырым
//	    cfg.AutoReloadMaxNCtx, то есть с env-значением. Типовой агентский путь
//	    (prompt_too_long) идёт именно через решатель, поэтому решение оператора
//	    обходилось.
//
// ПРАВИЛО СЕЙЧАС. Формула потолка одна — Proxy.effectiveAutoReloadCeiling.
// Решатель собственного мнения не имеет: ему передают готовое значение
// (ReloadCeiling), и он его использует во ВСЕХ ветках: отказ, кламп в
// prompt_too_long, кламп в n_ctx_needs_reload. Вторая формула в пакете
// запрещена — именно из-за неё правка «в одном месте» не работала.
//
// ПОЧЕМУ ИМЕННО max(env, заявленное), а не «всегда заявленное». Env-потолок —
// защита от АВТО-роста по клиентскому запросу (клиент не должен поднимать окно
// сам). Но если окно заявил ОПЕРАТОР в карточке модели, это его политика, и
// перебивать её нельзя: иначе настройка в WebUI выглядит неработающей — ровно
// то, на что жаловался оператор. Физика (VRAM/GGUF) проверяется отдельно,
// поэтому «в никуда» окно не растёт.

// ReloadCeiling — потолок авто-роста и его происхождение.
//
// Effective — итоговое значение (0 = ограничения нет).
// Env — сырое значение LB_NCTX_RELOAD_MAX_N_CTX (нужно ТОЛЬКО для атрибуции
// причины: «env» или «profile поднял потолок»). Решения по нему не принимаются.
type ReloadCeiling struct {
	Effective int
	Env       int
}

// source — что связало окно: физика, заявленное окно оператора или env-потолок.
//
// Пустая строка = ограничение не участвовало (окно не резалось). Имена
// стабильные: на них смотрят WebUI и оператор (`limited_by`).
func (ceil ReloadCeiling) source(physicalMax int) string {
	if physicalMax > 0 && ceil.Effective > 0 && physicalMax < ceil.Effective {
		return "physical"
	}
	switch {
	case ceil.Effective <= 0:
		return ""
	case ceil.Env > 0 && ceil.Effective > ceil.Env:
		// Потолок поднят заявленным окном оператора: режет уже не env.
		return "profile"
	default:
		return "env"
	}
}

// limitReason — текст причины отказа с числами и источником.
//
// Формат стабилен по смыслу (required / effective / env / physical), а не по
// точной строке: тесты и WebUI читают поля, а человек — текст.
func (ceil ReloadCeiling) limitReason(required, physicalMax int) string {
	if physicalMax > 0 && physicalMax < required {
		return nCtxLimitReason(required, ceil.Effective, physicalMax)
	}
	if ceil.Env > 0 && ceil.Effective > ceil.Env {
		return nCtxLimitReason(required, ceil.Effective, physicalMax) +
			"; limited_by=profile (заявленное окно оператора в карточке модели подняло потолок)"
	}
	return nCtxLimitReason(required, ceil.Effective, physicalMax) + "; limited_by=env (LB_NCTX_RELOAD_MAX_N_CTX)"
}

// ProfileEffectiveWindow — что реально применится к окну модели и почему.
//
// Отдаётся в API профиля (GET model-profiles, GET model-profiles/{name}), чтобы
// WebUI мог показать рядом с полем «окно»: «заявлено 256000, ограничено
// 131072, причина: env-потолок (LB_NCTX_RELOAD_MAX_N_CTX)». Без этого оператор
// видит только своё значение и не понимает, почему модель загрузилась меньше —
// это и была живая жалоба.
//
// physical не входит в расчёт: он зависит от backend'а и свободной VRAM в момент
// загрузки, поэтому проверяется на пути загрузки, а не в карточке профиля.
func (p *Proxy) ProfileEffectiveWindow(modelName string) (effective int, limitedBy, reason string, envCeiling int) {
	if p == nil || modelName == "" {
		return 0, "", "", 0
	}

	declared := 0
	if prof, ok := p.GetModelProfile(modelName); ok {
		slots := prof.Parallel
		if slots < 1 {
			slots = 1
		}
		if total, _, _ := resolveDeclaredWindow(prof.ContextLength, prof.ContextPerSeq, slots); total > 0 {
			declared = total
		}
	}

	if p.nctxReload != nil {
		envCeiling = p.nctxReload.Config().AutoReloadMaxNCtx
	}

	effective = declared
	if envCeiling > effective {
		effective = envCeiling
	}
	// Для АВТО-роста потолок — max(env, заявленное). Но фактическое окно модели —
	// это то, что заявлено в профиле; если заявленного нет, действует env.
	switch {
	case declared > 0 && envCeiling > declared:
		limitedBy = "env"
		reason = "окно модели задано " + strconv.Itoa(declared) +
			", но LB_NCTX_RELOAD_MAX_N_CTX=" + strconv.Itoa(envCeiling) +
			" разрешает авто-рост только до него"
	case declared > 0:
		limitedBy = ""
		reason = "окно задано оператором в карточке модели; env-потолок " +
			strconv.Itoa(envCeiling) + " не ограничивает (auto-growth разрешён до заявленного)"
	case envCeiling > 0:
		limitedBy = "env"
		reason = "окно не задано в профиле: действует LB_NCTX_RELOAD_MAX_N_CTX=" + strconv.Itoa(envCeiling)
	default:
		limitedBy = ""
		reason = "окно не ограничено авто-потолком (env не задан); предел проверит загрузка (VRAM/GGUF)"
	}
	return effective, limitedBy, reason, envCeiling
}
