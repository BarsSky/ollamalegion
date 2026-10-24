package i18n

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// bootstrap.go — R234: как процессы узнают язык.
//
// Порядок для воркеров (cppworker, agent):
//  1. <COMPONENT>_LOG_LANG (например CPPWORKER_LOG_LANG) — точечный over ride;
//  2. OLLAMALEGION_LOG_LANG / LB_LOG_LANG — общий для стенда;
//  3. локаль LANG/LC_ALL, иначе ru;
//  4. дальше StartFromBalancer подтягивает язык с балансера — источник истины:
//     переключение в WebUI меняет язык всех воркеров без перезапуска.
//
// Значения: ru | en | auto.

// BootstrapFromEnv применяет локальную настройку языка (шаги 1-3) и пишет в лог,
// откуда она взялась. Вызывается в самом начале main(), до остальных сообщений.
func BootstrapFromEnv(component string) Lang {
	names := []string{}
	if c := strings.ToUpper(strings.TrimSpace(component)); c != "" {
		names = append(names, c+"_LOG_LANG")
	}
	names = append(names, "OLLAMALEGION_LOG_LANG", "LB_LOG_LANG")

	lang, raw, src := FromEnv(names...)
	if src == "" {
		lang = FromLocale()
		Set(lang)
		log.Print(T("i18n.locale.fallback", lang))
		return lang
	}
	Set(lang)
	log.Print(T("i18n.env.override", lang, src, raw))
	return lang
}

// StartFromBalancer запускает фоновую синхронизацию языка с балансером.
// balancerURL — management-порт балансера (например http://loadbalancer:18081).
// Если адрес пуст, синхронизация не запускается: воркер остаётся на локальной
// настройке (это нормальный режим, когда балансер на другой машине и не указан).
//
// Интервал: env OLLAMALEGION_LOG_LANG_SYNC_SEC (по умолчанию 60 с, минимум 5 с).
func StartFromBalancer(ctx context.Context, balancerURL, token string) (context.CancelFunc, error) {
	if strings.TrimSpace(balancerURL) == "" {
		return func() {}, nil
	}
	interval := 60 * time.Second
	if v := strings.TrimSpace(os.Getenv("OLLAMALEGION_LOG_LANG_SYNC_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 {
			interval = time.Duration(n) * time.Second
		}
	}
	return NewRemoteSync(balancerURL, token, interval).Start(ctx)
}
