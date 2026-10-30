package balancer

// llama_upstream_r269.go — R269 (2026-10-29): отслеживание АКТУАЛЬНОЙ
// стабильной версии llama.cpp и «у вас устаревшая сборка».
//
// ЗАЧЕМ. У каждого узла своя сборка llama.cpp (env `CPPWORKER_LLAMA_BUILD` →
// types.Backend.BuildInfo, R101). Балансер уже сравнивает сборки МЕЖДУ СОБОЙ
// (протокол RPC версионно-чувствителен), но не знает, что вышло в upstream:
// оператор узнавал об обновлениях только вручную, а «почему у меня падает то,
// что у других работает» упиралось в неизвестную версию.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ:
//   - никаких запросов в hot path: опрос идёт фоновой петлёй по TTL, а
//     получатели читают кэш в памяти;
//   - никаких «догадок» о свежести: если сравнивать нечем (нет ни тега, ни
//     коммита, ни даты) — relation=unknown, а не «устарело». Ложное уведомление
//     «обновитесь» хуже молчания: оператор начинает чинить то, что не сломано;
//   - сети может не быть вовсе (закрытый стенд): ошибка не роняет ничего, кэш
//     остаётся прежним, в лог — одна строка на СМЕНУ состояния, без спама.
//
// ИСТОЧНИК. GitHub releases (по умолчанию `ggml-org/llama.cpp`): tag_name и
// published_at. Тег llama.cpp монотонный (`bNNNN`), поэтому сравнение по нему
// надёжно и не требует сети: достаточно одного «последнего» значения.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// llamaUpstreamFileName — кэш в том же writable-томе, что state.json и
	// профили: значение переживает рестарт балансера, и после перезапуска
	// уведомление не «пропадает до следующего опроса».
	llamaUpstreamFileName = "llama-upstream.json"

	// Дефолты. TTL 6 часов: llama.cpp выпускает сборки часто, но опрашивать
	// GitHub чаще — это лишний внешний трафик и риск упереться в rate limit.
	defaultLlamaUpstreamTTLSec     = 6 * 60 * 60
	defaultLlamaUpstreamTimeoutSec = 10
	defaultLlamaUpstreamRepo       = "ggml-org/llama.cpp"

	// Отношения версий (стабильные строки — их читает WebUI и API).
	llamaRelationUpToDate = "up_to_date"
	llamaRelationBehind   = "behind"
	llamaRelationAhead    = "ahead"
	llamaRelationUnknown  = "unknown"
)

// llamaUpstreamConfig — настройки опроса (env читается при каждом тике, чтобы
// оператор мог поменять TTL без пересборки образа).
type llamaUpstreamConfig struct {
	Enabled    bool
	TTLSec     int
	TimeoutSec int
	Repo       string
}

func llamaUpstreamConfigFromEnv() llamaUpstreamConfig {
	cfg := llamaUpstreamConfig{
		Enabled:    true,
		TTLSec:     defaultLlamaUpstreamTTLSec,
		TimeoutSec: defaultLlamaUpstreamTimeoutSec,
		Repo:       defaultLlamaUpstreamRepo,
	}
	if v, ok := os.LookupEnv("LB_LLAMA_UPSTREAM_CHECK_ENABLED"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off":
			cfg.Enabled = false
		}
	}
	// R278: TTL — «горячая» настройка (реестр: override из WebUI → env →
	// дефолт), поэтому её можно менять без пересборки и без рестарта: конфиг
	// перечитывается на каждом тике трекера.
	if n, ok := settings.HotInt("LB_LLAMA_UPSTREAM_TTL_SEC"); ok && n > 0 {
		cfg.TTLSec = int(n)
	}
	if n, ok := intFromEnv("LB_LLAMA_UPSTREAM_TIMEOUT_SEC"); ok && n > 0 {
		cfg.TimeoutSec = n
	}
	if v := strings.TrimSpace(os.Getenv("LB_LLAMA_UPSTREAM_REPO")); v != "" {
		cfg.Repo = v
	}
	return cfg
}

// intFromEnv — целое из env (0/мусор = «не задано»).
func intFromEnv(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// llamaUpstreamCache — то, что лежит на диске и в памяти.
type llamaUpstreamCache struct {
	Repo         string    `json:"repo"`
	LatestTag    string    `json:"latestTag,omitempty"`
	LatestCommit string    `json:"latestCommit,omitempty"`
	PublishedAt  time.Time `json:"publishedAt,omitempty"`
	FetchedAt    time.Time `json:"fetchedAt,omitempty"`
	// LastError — последняя ошибка опроса (для диагностики; не «состояние»).
	LastError string `json:"lastError,omitempty"`
}

// LlamaBuildStatus — вердикт по одному узлу. Поля JSON-стабильны: их читает
// WebUI и внешние потребители, поэтому имена не меняем.
type LlamaBuildStatus struct {
	Current      string `json:"current,omitempty"`
	LatestStable string `json:"latestStable,omitempty"`
	LatestTag    string `json:"latestTag,omitempty"`
	LatestCommit string `json:"latestCommit,omitempty"`
	PublishedAt  string `json:"publishedAt,omitempty"`
	Relation     string `json:"relation"`
	CheckedAt    string `json:"checkedAt,omitempty"`
	Source       string `json:"source,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// LlamaUpstreamSnapshot — агрегат по кластеру (для /api/v1/llama-version).
type LlamaUpstreamSnapshot struct {
	Enabled      bool               `json:"enabled"`
	Repo         string             `json:"repo"`
	LatestStable string             `json:"latestStable,omitempty"`
	LatestTag    string             `json:"latestTag,omitempty"`
	LatestCommit string             `json:"latestCommit,omitempty"`
	PublishedAt  string             `json:"publishedAt,omitempty"`
	CheckedAt    string             `json:"checkedAt,omitempty"`
	Relation     string             `json:"relation"`
	Error        string             `json:"error,omitempty"`
	Behind       []string           `json:"behindBackends,omitempty"`
	Backends     []LlamaNodeVersion `json:"backends"`
}

// LlamaNodeVersion — версия узла в агрегате.
type LlamaNodeVersion struct {
	ID       string `json:"id"`
	Current  string `json:"current,omitempty"`
	Relation string `json:"relation"`
	Reason   string `json:"reason,omitempty"`
}

// llamaUpstreamTracker — фоновая петля + кэш.
type llamaUpstreamTracker struct {
	mu       sync.RWMutex
	cache    llamaUpstreamCache
	path     string
	stopCh   chan struct{}
	stopOnce sync.Once
	// stateKey — «последнее известное состояние», чтобы писать в лог только на
	// смену (иначе ровно та же ошибка сети каждые 6 часов = шум в логах).
	stateKey     string
	stateKeyOnce sync.Once
}

// llamaUpstreamCachePath — рядом со state.json, в том же writable-томе.
func llamaUpstreamCachePath(statePath string) string {
	dir := filepath.Dir(statePath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	return filepath.Join(dir, llamaUpstreamFileName)
}

// startLlamaUpstreamTracking — ленивый старт (вызывается из NewProxy).
func (p *Proxy) startLlamaUpstreamTracking() {
	if p == nil {
		return
	}
	t := &llamaUpstreamTracker{
		path:   llamaUpstreamCachePath(p.statePath),
		stopCh: make(chan struct{}),
	}
	// Пустой statePath (юнит-тесты, урезанный прокси) — кэш только в памяти:
	// иначе тесты писали бы в data/ рабочего дерева.
	if strings.TrimSpace(p.statePath) == "" {
		t.path = ""
	}
	t.loadFromDisk()
	p.llamaUpstream = t
	go t.loop(p)
}

// stopLlamaUpstreamTracking — остановка петли (Shutdown).
func (p *Proxy) stopLlamaUpstreamTracking() {
	if p == nil || p.llamaUpstream == nil {
		return
	}
	p.llamaUpstream.Stop()
}

// Stop — идемпотентная остановка.
func (t *llamaUpstreamTracker) Stop() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stopCh) })
}

// loop — проверка «пора ли опрашивать» раз в минуту. Тик дешёвый: без сети он
// только сравнивает время, а опрос случается не чаще TTL.
func (t *llamaUpstreamTracker) loop(p *Proxy) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.maybeRefresh(p, false)
		}
	}
}

// maybeRefresh — опрос upstream, если истёк TTL (force=true — всегда).
func (t *llamaUpstreamTracker) maybeRefresh(p *Proxy, force bool) {
	cfg := llamaUpstreamConfigFromEnv()
	if !cfg.Enabled {
		return
	}
	t.mu.RLock()
	fetchedAt := t.cache.FetchedAt
	sameRepo := t.cache.Repo == cfg.Repo
	t.mu.RUnlock()

	if !force && sameRepo && time.Since(fetchedAt) < time.Duration(cfg.TTLSec)*time.Second {
		return
	}

	tag, commit, published, err := fetchLatestLlamaRelease(cfg)
	if err != nil {
		t.recordError(cfg.Repo, err)
		return
	}
	t.recordSuccess(cfg, tag, commit, published)
}

// recordSuccess — сохранить результат и (при смене) записать в лог и на диск.
func (t *llamaUpstreamTracker) recordSuccess(cfg llamaUpstreamConfig, tag, commit string, published time.Time) {
	t.mu.Lock()
	prevTag := t.cache.LatestTag
	t.cache = llamaUpstreamCache{
		Repo:         cfg.Repo,
		LatestTag:    tag,
		LatestCommit: commit,
		PublishedAt:  published,
		FetchedAt:    time.Now().UTC(),
	}
	snapshot := t.cache
	key := "ok:" + cfg.Repo + ":" + tag
	changed := t.stateKey != key
	t.stateKey = key
	t.mu.Unlock()

	if changed && (prevTag != tag) {
		logger.Get().Infow("llama upstream: latest stable release known",
			"repo", cfg.Repo, "tag", tag, "commit", commit, "published", published.Format(time.RFC3339))
	}
	t.persist(snapshot)
}

// recordError — ошибка опроса: кэш НЕ трогаем (прежнее значение остаётся
// валидным «последним известным»), пишем причину и логируем только на смену.
func (t *llamaUpstreamTracker) recordError(repo string, err error) {
	t.mu.Lock()
	t.cache.LastError = err.Error()
	key := "err:" + repo + ":" + err.Error()
	changed := t.stateKey != key
	t.stateKey = key
	snapshot := t.cache
	t.mu.Unlock()

	if changed {
		logger.Get().Warnw("llama upstream: check failed (offline or blocked?)",
			"repo", repo, "error", err.Error(),
			"hint", "проверка не критична: собственные версии узлов и их сравнение работают без сети",
			"env", "LB_LLAMA_UPSTREAM_CHECK_ENABLED=0 отключает опрос")
	}
	t.persist(snapshot)
}

// persist — записать кэш на диск (best-effort).
func (t *llamaUpstreamTracker) persist(c llamaUpstreamCache) {
	if t.path == "" {
		return
	}
	if dir := filepath.Dir(t.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, t.path)
}

// loadFromDisk — поднять кэш (без сети). Битый файл не мешает старту.
func (t *llamaUpstreamTracker) loadFromDisk() {
	if t.path == "" {
		return
	}
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var c llamaUpstreamCache
	if err := json.Unmarshal(data, &c); err != nil {
		logger.Get().Warnw("llama upstream: cache file is not valid json, ignoring", "path", t.path)
		return
	}
	t.mu.Lock()
	t.cache = c
	t.mu.Unlock()
}

// llamaUpstreamAPIBase — база GitHub API. Переменная, а не константа в строке:
// тесты подменяют её на httptest-сервер, чтобы проверять и успех, и оффлайн,
// не ходя в интернет (иначе тест зависит от сети и от rate limit GitHub).
var llamaUpstreamAPIBase = "https://api.github.com"

// fetchLatestLlamaRelease — запрос последнего релиза в GitHub.
func fetchLatestLlamaRelease(cfg llamaUpstreamConfig) (tag, commit string, published time.Time, err error) {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultLlamaUpstreamTimeoutSec * time.Second
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", llamaUpstreamAPIBase, strings.TrimSpace(cfg.Repo))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ollama-loadbalancer/llama-upstream-check")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", time.Time{}, fmt.Errorf("github api: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Target      string    `json:"target_commitish"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", time.Time{}, err
	}
	tag = strings.TrimSpace(body.TagName)
	if tag == "" {
		return "", "", time.Time{}, errors.New("github api: empty tag_name")
	}
	commit = strings.TrimSpace(body.Target)
	// target_commitish у релизов llama.cpp — обычно имя ветки, а не SHA.
	// Не выдаём ветку за коммит: иначе сравнение «по коммиту» станет ложью.
	if !looksLikeCommit(commit) {
		commit = ""
	}
	return tag, commit, body.PublishedAt.UTC(), nil
}

// looksLikeCommit — строка похожа на git-SHA (короткий или полный).
var commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func looksLikeCommit(s string) bool {
	return commitRe.MatchString(strings.ToLower(strings.TrimSpace(s)))
}

// tagNumberRe — номер сборки llama.cpp (`b4321`): теги монотонны, поэтому
// сравнение по номеру надёжно и не требует даты.
var tagNumberRe = regexp.MustCompile(`(?i)\bb(\d{3,})\b`)

// llamaBuildParts — разобранная идентичность сборки.
type llamaBuildParts struct {
	Tag    string
	Commit string
	Date   time.Time
}

// parseLlamaBuild — терпимый разбор строки сборки.
//
// Форматы, которые встречаются на стендах: `b4321`, `b4321-abcdef1`,
// `abcdef1234`, `2026-09-01`, `build 2026-09-01 (b4321)`. Терпимость здесь не
// «на всякий случай»: строка приходит из env, её пишет оператор/образ.
func parseLlamaBuild(raw string) llamaBuildParts {
	out := llamaBuildParts{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	if m := tagNumberRe.FindStringSubmatch(raw); len(m) == 2 {
		out.Tag = "b" + m[1]
	}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == '(' || r == ')' || r == ',' || r == '-' || r == '_' || r == '/'
	}) {
		low := strings.ToLower(field)
		if looksLikeCommit(low) && (out.Tag == "" || low != strings.ToLower(out.Tag)) {
			out.Commit = low
			break
		}
	}
	if d := parseBuildDate(raw); !d.IsZero() {
		out.Date = d
	}
	return out
}

// dateRe — дата в строке сборки (YYYY-MM-DD).
var dateRe = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)

func parseBuildDate(raw string) time.Time {
	m := dateRe.FindStringSubmatch(raw)
	if len(m) != 4 {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// CompareLlamaBuilds — отношение «сборка узла» к «последней стабильной».
//
// Порядок проверок содержательный:
//  1. нет одной из сторон → unknown (сравнивать нечего);
//  2. совпал коммит или тег → up_to_date;
//  3. оба тега `bNNNN` → сравнение номеров (теги монотонны);
//  4. обе даты известны → по дате (позже = ahead, раньше = behind);
//  5. иначе → unknown. Именно здесь важно НЕ угадывать: «устарело» на
//     неизвестной версии заставит оператора обновляться зря.
func CompareLlamaBuilds(current, latest string) (relation, reason string) {
	cur := parseLlamaBuild(current)
	lat := parseLlamaBuild(latest)
	if strings.TrimSpace(current) == "" || strings.TrimSpace(latest) == "" {
		return llamaRelationUnknown, "version missing on one side"
	}
	// R305 (2026-10-30): коммиты сравниваем по ПРЕФИКСУ.
	//
	// ЖИВАЯ ЖАЛОБА. Узел сообщает короткий коммит (`0eadefebd`), GitHub — полный
	// (`8642541b255bfb5d…`). Строгое равенство строк объявляло одинаковые сборки
	// разными — а разным быть не могут, это один и тот же git-объект в двух
	// формах записи. Сравниваем 7-символьный префикс: этого достаточно, чтобы
	// различить коммиты (git сам так делает), и не нужно гадать длину.
	if cur.Commit != "" && lat.Commit != "" && sameCommitPrefix(cur.Commit, lat.Commit) {
		return llamaRelationUpToDate, "same commit"
	}
	if cur.Tag != "" && lat.Tag != "" && cur.Tag == lat.Tag {
		return llamaRelationUpToDate, "same tag"
	}
	if cur.Tag != "" && lat.Tag != "" && cur.Tag != lat.Tag {
		cn, cok := tagToInt(cur.Tag)
		ln, lok := tagToInt(lat.Tag)
		if cok && lok {
			switch {
			case cn < ln:
				return llamaRelationBehind, "tag number lower"
			case cn > ln:
				return llamaRelationAhead, "tag number higher"
			default:
				return llamaRelationUpToDate, "same tag number"
			}
		}
	}
	if !cur.Date.IsZero() && !lat.Date.IsZero() {
		switch {
		case cur.Date.Before(lat.Date):
			return llamaRelationBehind, "build date earlier than release"
		case cur.Date.After(lat.Date):
			return llamaRelationAhead, "build date later than release"
		default:
			return llamaRelationUpToDate, "same date"
		}
	}
	if cur.Commit != "" && lat.Commit != "" {
		return llamaRelationUnknown, "different commits, no comparable version"
	}
	return llamaRelationUnknown, "no comparable version"
}

// sameCommitPrefix — один ли это коммит (сравнение по короткому префиксу).
//
// 7 символов — минимальная длина, которую git считает однозначной в репозитории
// llama.cpp; более короткие строки не считаем коммитами вовсе (иначе `b4321` и
// `0abc` давали бы ложные совпадения).
func sameCommitPrefix(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	const minLen = 7
	if len(a) < minLen || len(b) < minLen {
		return a == b
	}
	n := minLen
	if len(a) < n {
		n = len(a)
	}
	if len(b) < n {
		n = len(b)
	}
	return a[:n] == b[:n]
}

// tagToInt — номер из тега `bNNNN`.
func tagToInt(tag string) (int, bool) {
	m := tagNumberRe.FindStringSubmatch(tag)
	if len(m) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// LlamaUpstreamEnabled — включён ли опрос (для API/WebUI).
func (p *Proxy) LlamaUpstreamEnabled() bool {
	return llamaUpstreamConfigFromEnv().Enabled
}

// LlamaBuildStatusFor — вердикт по конкретному бэкенду.
func (p *Proxy) LlamaBuildStatusFor(backendID string) LlamaBuildStatus {
	if p == nil {
		return LlamaBuildStatus{Relation: llamaRelationUnknown, Reason: "no proxy"}
	}
	cfg := llamaUpstreamConfigFromEnv()
	status := LlamaBuildStatus{
		Relation: llamaRelationUnknown,
		Source:   cfg.Repo,
	}
	if b := p.GetBackend(backendID); b != nil {
		status.Current = strings.TrimSpace(b.BuildInfo)
	}
	if !cfg.Enabled {
		status.Reason = "upstream check disabled"
		return status
	}
	latest, fetchedAt, published, errText, ok := p.llamaUpstreamSnapshot()
	status.LatestStable = latest
	if l := parseLlamaBuild(latest); l.Tag != "" {
		status.LatestTag = l.Tag
	}
	if l := parseLlamaBuild(latest); l.Commit != "" {
		status.LatestCommit = l.Commit
	}
	if !fetchedAt.IsZero() {
		status.CheckedAt = fetchedAt.UTC().Format(time.RFC3339)
	}
	// R305 (2026-10-30): дата публикации релиза теперь ДОХОДИТ до ответа.
	//
	// Поле publishedAt в статусе было объявлено с R269 и всегда оставалось
	// пустым: дата лежала в кэше, но наружу не отдавалась. Оператор видел
	// «последний стабильный: v0.8.0 (коммит)» и не мог понять, свежий ли это
	// релиз относительно его сборки — а без даты и сравнение не считалось.
	// Заполняем ДО сравнения: дата нужна и для вердикта, и для отчёта.
	if !published.IsZero() {
		status.PublishedAt = published.UTC().Format(time.RFC3339)
	}
	if errText != "" {
		status.Reason = "last check failed: " + errText
	}
	if !ok {
		if status.Reason == "" {
			status.Reason = "latest stable version unknown (no successful check yet)"
		}
		return status
	}
	// R305: к строке релиза добавляем дату публикации (только для сравнения).
	//
	// ЗАЧЕМ. У сборки узла теперь есть дата коммита, у релиза — дата публикации;
	// без передачи даты сравнение не могло дойти до шага «по дате» и честно
	// заканчивалось unknown. Отображаемое поле LatestStable остаётся прежним:
	// дата показывается отдельно (publishedAt), а не внутри строки версии.
	latestForCompare := latest
	if l := parseLlamaBuild(latest); l.Date.IsZero() {
		if pub := parseLlamaBuild(status.PublishedAt); !pub.Date.IsZero() {
			latestForCompare = latest + " " + pub.Date.Format("2006-01-02")
		}
	}
	rel, why := CompareLlamaBuilds(status.Current, latestForCompare)
	status.Relation = rel
	if why != "" {
		if status.Reason != "" {
			status.Reason = why + "; " + status.Reason
		} else {
			status.Reason = why
		}
	}
	return status
}

// llamaUpstreamLatest — снимок кэша.
func (p *Proxy) llamaUpstreamLatest() (latest string, fetchedAt time.Time, errText string, ok bool) {
	latest, fetchedAt, _, errText, ok = p.llamaUpstreamSnapshot()
	return latest, fetchedAt, errText, ok
}

// llamaUpstreamSnapshot — тот же снимок, но с датой публикации релиза.
//
// Отдельная функция, а не ещё один возвращаемый параметр в llamaUpstreamLatest:
// у того уже четыре значения, и пятое сделало бы каждый вызов нечитаемым
// (`_, _, _, _, _ =`). Здесь вызывающих два — статус узла и агрегат.
func (p *Proxy) llamaUpstreamSnapshot() (latest string, fetchedAt, published time.Time, errText string, ok bool) {
	if p == nil || p.llamaUpstream == nil {
		return "", time.Time{}, time.Time{}, "", false
	}
	t := p.llamaUpstream
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.cache.LatestTag == "" || t.cache.FetchedAt.IsZero() {
		return "", t.cache.FetchedAt, t.cache.PublishedAt, t.cache.LastError, false
	}
	latest = t.cache.LatestTag
	if t.cache.LatestCommit != "" {
		latest += " (" + t.cache.LatestCommit + ")"
	}
	return latest, t.cache.FetchedAt, t.cache.PublishedAt, t.cache.LastError, true
}

// LlamaUpstreamSnapshot — агрегат по кластеру.
func (p *Proxy) LlamaUpstreamSnapshot() LlamaUpstreamSnapshot {
	cfg := llamaUpstreamConfigFromEnv()
	snap := LlamaUpstreamSnapshot{
		Enabled:  cfg.Enabled,
		Repo:     cfg.Repo,
		Relation: llamaRelationUnknown,
		Backends: []LlamaNodeVersion{},
	}
	if p == nil {
		return snap
	}
	if cfg.Enabled {
		if latest, fetchedAt, _, ok := p.llamaUpstreamLatest(); ok {
			snap.LatestStable = latest
			if l := parseLlamaBuild(latest); l.Tag != "" {
				snap.LatestTag = l.Tag
			}
			if l := parseLlamaBuild(latest); l.Commit != "" {
				snap.LatestCommit = l.Commit
			}
			snap.CheckedAt = fetchedAt.UTC().Format(time.RFC3339)
			// R305: дата публикации релиза — в агрегат, тем же правилом, что и в
			// статусе узла: поле объявлено, но не заполнялось.
			if _, _, published, _, okPub := p.llamaUpstreamSnapshot(); okPub && !published.IsZero() {
				snap.PublishedAt = published.UTC().Format(time.RFC3339)
			}
		}
		// Ошибку показываем отдельным полем, а не вместо версии: прежнее
		// успешное значение остаётся валидным «последним известным», и
		// уведомление «устарело» на нём всё ещё осмысленно.
		if _, _, errText, _ := p.llamaUpstreamLatest(); errText != "" {
			snap.Error = errText
		}
	}

	all := p.GetAllBackends()
	worst := llamaRelationUnknown
	for i := range all {
		b := &all[i]
		if b.Type != types.BackendTypeLlamaCpp {
			continue
		}
		st := p.LlamaBuildStatusFor(b.ID)
		snap.Backends = append(snap.Backends, LlamaNodeVersion{
			ID:       b.ID,
			Current:  st.Current,
			Relation: st.Relation,
			Reason:   st.Reason,
		})
		if st.Relation == llamaRelationBehind {
			snap.Behind = append(snap.Behind, b.ID)
			worst = llamaRelationBehind
		} else if st.Relation == llamaRelationUpToDate && worst == llamaRelationUnknown {
			worst = llamaRelationUpToDate
		}
	}
	snap.Relation = worst
	return snap
}
