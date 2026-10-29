// auth_session.go — R201 (2026-10-21): ПОЛНОЦЕННАЯ АУТЕНТИФИКАЦИЯ WEBUI.
//
// ЗАЧЕМ. Раньше единственным «входом» был X-API-Token, вшитый в страницу
// (WEBUI_CONFIG.API_TOKEN): любой, кто открыл WebUI из локальной сети, получал
// админский доступ. Пользовательского входа не было, ролей не было, и «админ
// зашёл с другой машины» означало «любой зашёл как админ».
//
// МОДЕЛЬ:
//   - пользователи в файле LB_USERS_FILE (права 0600). В compose-стеке это
//     /app/data/users.json на томе balancer_data; если переменная НЕ задана —
//     относительный config/users.json В КОНТЕЙНЕРЕ (ефемерный: пропадёт при
//     пересоздании). Пароли — только хеши PBKDF2-HMAC-SHA256 (stdlib);
//   - серверные сессии: cookie ol_session, HttpOnly + SameSite=Lax (+Secure при
//     HTTPS), TTL 12 часов со скользящим продлением, id — 32 случайных байта;
//   - роли admin (всё) и viewer (только чтение; мутирующие запросы → 403);
//   - CSRF: для сессионных мутирующих запросов обязателен X-CSRF-Token,
//     значение отдаётся в GET /api/v1/auth/me;
//   - X-API-Token остаётся валидным (break-glass для Cline/скриптов и на случай
//     потери пароля), но больше не вшивается в страницу.
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// --- пароли ---------------------------------------------------------------

const (
	pbkdf2Iterations = 120000
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
	usersFileName    = "users.json"
)

// pbkdf2SHA256 — PBKDF2-HMAC-SHA256 (RFC 8018), реализация на stdlib.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// HashPassword — строка вида pbkdf2$<iter>$<saltB64>$<hashB64>.
func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", os.ErrInvalid
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return "pbkdf2$" + strconv.Itoa(pbkdf2Iterations) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(sum), nil
}

// VerifyPassword — сверка пароля с хешем (constant-time).
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 5000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- пользователи ---------------------------------------------------------

// AuthUser — запись пользователя (пароль только хешем).
type AuthUser struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	PasswordHash string `json:"password_hash"`
}

// UserStore — файл пользователей с горячей перезагрузкой при изменении.
type UserStore struct {
	path string
	mu   sync.RWMutex
	list []AuthUser
	// R270 (2026-10-29): ПРОИСХОЖДЕНИЕ УЧЁТНОЙ ЗАПИСИ — явно и проверяемо.
	//
	// ЖИВАЯ ЖАЛОБА (оффлайн-стенд): оператор задал LB_ADMIN_USER/LB_ADMIN_PASSWORD
	// в env, но войти не смог, и причину неоткуда было узнать: ни в логе, ни в
	// /auth/me не было сказано, применён ли env-админ, лежит ли он на диске и
	// вообще доступен ли файл пользователей на запись. На изолированной машине
	// это критично: там нет UI-мастера, нет сети, и единственный вход — env.
	// Теперь происхождение и «переживёт ли рестарт» отдаются в /auth/me и в лог.
	boot bootstrapInfo
}

// bootstrapInfo — что стало источником входа при старте.
//
// Source: "env" (создан из env при пустом/отсутствующем файле), "file" (взят из
// файла), "env+file" (файл был, env-пароль применён/обновлён), "none".
// Persisted: удалось ли записать файл (ложь = запись не удалась, учётка живёт
// только в памяти и пропадёт при рестарте — типовой симптом «сегодня пускает,
// завтра нет»). Writable: можно ли вообще писать в файл/каталог.
type bootstrapInfo struct {
	Source     string `json:"source"`
	EnvApplied bool   `json:"envApplied"`
	Persisted  bool   `json:"persisted"`
	Writable   bool   `json:"writable"`
	Path       string `json:"path"`
	Users      int    `json:"users"`
	Reason     string `json:"reason,omitempty"`
}

// BootstrapInfo — снимок для /auth/me (секретов здесь нет и быть не должно).
func (u *UserStore) BootstrapInfo() bootstrapInfo {
	u.mu.RLock()
	defer u.mu.RUnlock()
	info := u.boot
	info.Users = len(u.list)
	return info
}

// pathWritable — можно ли писать файл пользователей (R270).
//
// Проверяем ДЕЛОМ, а не правами на бумаге: на оффлайн-стенде типовой дефект —
// файл смонтирован read-only (или каталог принадлежит другому пользователю), и
// тогда env-админ создаётся только в памяти. Без этой проверки оператор видел
// «вход работает» до пересоздания контейнера.
func pathWritable(path string) bool {
	if path == "" {
		return false
	}
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_ = f.Close()
		return true
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wtest-*")
	if err != nil {
		return false
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name)
	return true
}

// NewUserStore — загрузить пользователей.
//
// Если файла нет, но заданы LB_ADMIN_USER/LB_ADMIN_PASSWORD, создаём файл с одним
// администратором: это безопасный способ первого запуска без UI-мастера.
// Пустой пароль в env игнорируется (иначе можно случайно открыть панель).
func NewUserStore(path string) *UserStore {
	if path == "" {
		path = usersFileName
	}
	us := &UserStore{path: path}
	us.load()
	// R270: фиксируем, можно ли вообще писать файл — до любых попыток сохранить.
	// На оффлайн-стенде именно это отличает «админ применён» от «админ живёт до
	// первого рестарта».
	writable := pathWritable(path)
	us.boot = bootstrapInfo{
		Source:   "none",
		Path:     path,
		Writable: writable,
	}
	hadFromFile := len(us.list) > 0
	// R203 (2026-10-21): ЯВНО ЗАДАННЫЙ В .env ПАРОЛЬ АДМИНИСТРАТОРА ПОБЕЖДАЕТ.
	//
	// ЖИВОЙ СЛУЧАЙ: оператор сменил LB_ADMIN_PASSWORD в deployments/.env, но users.json
	// уже существовал — новый пароль молча игнорировался, и войти можно было только
	// старым. По доктрине «явное действие оператора применяется» сверяем env-пароль с
	// сохранённым хешем и, если он другой, обновляем хеш (и гасим старые сессии —
	// они принадлежат прежнему паролю).
	us.syncAdminFromEnv()

	if len(us.list) == 0 {
		user := strings.TrimSpace(os.Getenv("LB_ADMIN_USER"))
		pass := os.Getenv("LB_ADMIN_PASSWORD")
		if user != "" && strings.TrimSpace(pass) != "" {
			if hash, err := HashPassword(pass); err == nil {
				us.list = []AuthUser{{Name: user, Role: "admin", PasswordHash: hash}}
				// R229: ошибка записи РАНЬШЕ глоталась. Живой симптом: оператор задал
				// LB_ADMIN_* и ждёт users.json — файла нет, в логе тишина, вход «работает»
				// до пересоздания контейнера и пропадает после. Теперь причина видна.
				if saveErr := us.saveLocked(); saveErr != nil {
					us.boot.Persisted = false
					us.boot.Reason = saveErr.Error()
					if logger.Get() != nil {
						logger.Get().Warnw(i18n.T("api.auth_session.admin_memory_only"),
							"path", us.path, "error", saveErr,
							"hint", i18n.T("api.auth_session.admin_hint_mount_volume"))
					}
				} else {
					us.boot.Persisted = true
				}
				us.boot.Source = "env"
				us.boot.EnvApplied = true
				if logger := logger.Get(); logger != nil {
					logger.Warnw(i18n.T("api.auth_session.admin_created_from_env"), "user", user)
				}
			}
		} else if user != "" || strings.TrimSpace(pass) != "" {
			// R270: половина пары — почти всегда опечатка в .env (забыли пароль или
			// имя). Молчать нельзя: оператор ждёт вход, а его нет.
			us.boot.Reason = "LB_ADMIN_USER/LB_ADMIN_PASSWORD заданы не полностью"
		}
	} else {
		// R270: источник различаем по ФАКТУ, а не по порядку веток. Сначала
		// выполняется syncAdminFromEnv, поэтому админ, созданный из env при
		// отсутствующем файле, попадает сюда же — но источник у него «env», а не
		// «file»: иначе оператор на оффлайн-стенде не отличит «взято из файла»
		// от «взято из моего .env», и правка .env снова станет загадкой.
		switch {
		case us.boot.EnvApplied && !hadFromFile:
			us.boot.Source = "env"
		case us.boot.EnvApplied:
			us.boot.Source = "env+file"
		default:
			us.boot.Source = "file"
		}
		if !hadFromFile && us.boot.Reason == "" {
			us.boot.Reason = "файл пользователей создан при старте"
		}
	}
	if us.boot.Persisted == false && us.boot.Source != "none" && us.boot.Reason == "" {
		us.boot.Persisted = us.boot.Writable
	}
	// R270: одна строка при старте — чтобы «почему не пускает» отвечал лог, а не
	// догадки. Пароль не печатается никогда.
	if logger := logger.Get(); logger != nil {
		logger.Infow("auth bootstrap",
			"source", us.boot.Source,
			"env_applied", us.boot.EnvApplied,
			"users", len(us.list),
			"path", us.path,
			"writable", us.boot.Writable,
			"persisted", us.boot.Persisted,
			"reason", us.boot.Reason)
	}
	if len(us.list) > 0 && !us.boot.Persisted {
		logger.Get().Warnw("auth: admin применяется только в памяти — файл пользователей не записан (вход пропадёт после рестарта контейнера)",
			"path", us.path, "writable", us.boot.Writable, "reason", us.boot.Reason)
	}
	return us
}

// Path — путь файла пользователей.
func (u *UserStore) Path() string { return u.path }

// adminEnvSyncEnabled — R275: применять ли env-учётку к уже существующим
// пользователям.
//
// ЗАЧЕМ ПЕРЕКЛЮЧАТЕЛЬ. Синк из env — это запись в файл пользователей: он может
// создать второго администратора или сменить пароль тому, кого оператор завёл
// руками. Для стенда это ровно то, что нужно («я задал пароль в .env — он должен
// работать»), но на общем/разделяемом стенде такое поведение может быть
// нежелательным, поэтому его можно выключить явно: LB_ADMIN_ENV_SYNC=0.
//
// ВАЖНО: выключение НЕ мешает первому запуску — создание администратора при
// пустом файле остаётся (иначе на изолированной машине войти было бы некому).
func adminEnvSyncEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LB_ADMIN_ENV_SYNC"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// syncAdminFromEnv — применить LB_ADMIN_USER/LB_ADMIN_PASSWORD к существующему
// администратору, если пароль в окружении изменился. Явное действие оператора
// (смена пароля в .env) должно применяться, иначе панель «не пускает новым паролем».
//
// R275 (2026-10-29). ЖИВОЙ ФАКТ со стенда r273: env задан (`LB_ADMIN_USER=skyadmin`
// и пароль — проверено `docker exec … env`), файл пользователей содержит того же
// админа с тем же паролем, вход РАБОТАЕТ, а `/api/v1/auth/me` отдаёт
// `bootstrap{source:"file", envApplied:false}`. Причина: ветка «пароль совпал»
// выходила из функции ДО того, как выставить EnvApplied, и оператор получал
// ответ «твой env не применён» ровно в самом благополучном случае. На оффлайн-
// стенде это прямая дорога к раскопкам: человек ищет, почему .env «не работает»,
// хотя учётка из .env действует.
//
// Теперь «совпало» — это тоже применение env, и оно так и называется.
func (u *UserStore) syncAdminFromEnv() {
	user := strings.TrimSpace(os.Getenv("LB_ADMIN_USER"))
	pass := os.Getenv("LB_ADMIN_PASSWORD")
	if user == "" || strings.TrimSpace(pass) == "" {
		return
	}
	if !adminEnvSyncEnabled() && len(u.list) > 0 {
		// Файл уже кем-то наполнен, а синк выключен оператором: не трогаем записи,
		// но говорим причину, иначе «env задан, а изменений нет» снова выглядит багом.
		u.mu.Lock()
		u.boot.Reason = "LB_ADMIN_ENV_SYNC=0 — env-учётка не применяется к существующему файлу пользователей"
		u.mu.Unlock()
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	// Если такой пользователь уже есть — сверяем пароль и при отличии обновляем.
	for i := range u.list {
		if !strings.EqualFold(u.list[i].Name, user) {
			continue
		}
		if VerifyPassword(u.list[i].PasswordHash, pass) {
			// R275: пароль из env УЖЕ действует — это применение env, а не «файл».
			// Persisted=true: запись существует на диске (мы её только что прочли),
			// перезапись не нужна и не делается — лишних записей в файл нет.
			u.boot.EnvApplied = true
			u.boot.Persisted = true
			if u.boot.Reason == "" {
				u.boot.Reason = "env-учётка совпала с записью в файле — пароль из .env действует"
			}
			return
		}
		hash, err := HashPassword(pass)
		if err != nil {
			return
		}
		u.list[i].PasswordHash = hash
		u.list[i].Role = "admin"
		// R270: фиксируем, что env реально применён, и удалось ли это сохранить —
		// иначе оператор на оффлайн-стенде не знает, действует его новый пароль
		// из .env или нет.
		u.boot.EnvApplied = true
		if err := u.saveLocked(); err != nil {
			u.boot.Persisted = false
			u.boot.Reason = err.Error()
			if logger.Get() != nil {
				logger.Get().Errorw(i18n.T("api.auth_session.admin_password_apply_failed"), "error", err)
			}
			return
		}
		u.boot.Persisted = true
		if logger.Get() != nil {
			logger.Get().Warnw(i18n.T("api.auth_session.admin_password_updated"), "user", u.list[i].Name)
		}
		return
	}

	// Такого пользователя нет (оператор сменил имя в .env) — создаём администратора.
	hash, err := HashPassword(pass)
	if err != nil {
		return
	}
	u.list = append(u.list, AuthUser{Name: user, Role: "admin", PasswordHash: hash})
	u.boot.EnvApplied = true
	if err := u.saveLocked(); err != nil {
		u.boot.Persisted = false
		u.boot.Reason = err.Error()
		if logger.Get() != nil {
			logger.Get().Errorw(i18n.T("api.auth_session.admin_create_failed"), "error", err)
		}
		return
	}
	u.boot.Persisted = true
	if logger.Get() != nil {
		logger.Get().Warnw(i18n.T("api.auth_session.admin_created_from_env_note"), "user", user,
			"note", i18n.T("api.auth_session.admin_created_others_hint"))
	}
}

// load — прочитать файл (ошибки только логируются: панель не должна падать).
func (u *UserStore) load() {
	raw, err := os.ReadFile(u.path)
	if err != nil {
		if !os.IsNotExist(err) && logger.Get() != nil {
			logger.Get().Warnw(i18n.T("api.auth_session.users_read_failed"), "path", u.path, "error", err)
		}
		return
	}
	var doc struct {
		Users []AuthUser `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		if logger.Get() != nil {
			logger.Get().Errorw(i18n.T("api.auth_session.users_parse_failed"), "path", u.path, "error", err)
		}
		return
	}
	u.mu.Lock()
	u.list = doc.Users
	u.mu.Unlock()
}

// saveLocked — записать файл (0600). Вызывается под mu.
func (u *UserStore) saveLocked() error {
	doc := map[string]interface{}{"users": u.list}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(u.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.path)
}

// Enabled — есть ли хотя бы один пользователь (иначе вход недоступен и WebUI
// обязан показать экран первого запуска).
func (u *UserStore) Enabled() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.list) > 0
}

// Verify — проверка пары имя/пароль. Возвращает пользователя при успехе.
func (u *UserStore) Verify(name, password string) (AuthUser, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, usr := range u.list {
		if !strings.EqualFold(usr.Name, strings.TrimSpace(name)) {
			continue
		}
		if VerifyPassword(usr.PasswordHash, password) {
			return usr, true
		}
		return AuthUser{}, false
	}
	return AuthUser{}, false
}

// List — имена и роли (без хешей) для админского GET.
func (u *UserStore) List() []map[string]string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]map[string]string, 0, len(u.list))
	for _, usr := range u.list {
		out = append(out, map[string]string{"name": usr.Name, "role": usr.Role})
	}
	return out
}

// Upsert — создать пользователя или сменить пароль/роль.
// minPasswordLength — минимальная длина пароля. В JS проверка была и раньше, но
// сервер её не знал: через API можно было поставить пароль из одного символа.
const minPasswordLength = 8

func (u *UserStore) Upsert(name, password, role string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return os.ErrInvalid
	}
	if strings.TrimSpace(password) != "" && len([]rune(password)) < minPasswordLength {
		return fmt.Errorf("%s", i18n.T("api.auth_session.password_too_short", minPasswordLength))
	}
	if role != "admin" && role != "viewer" {
		role = "viewer"
	}
	var hash string
	if strings.TrimSpace(password) != "" {
		h, err := HashPassword(password)
		if err != nil {
			return err
		}
		hash = h
	} else {
		// Пароль не задан — меняем только роль существующего пользователя.
		u.mu.RLock()
		for _, usr := range u.list {
			if strings.EqualFold(usr.Name, name) {
				hash = usr.PasswordHash
				break
			}
		}
		u.mu.RUnlock()
		if hash == "" {
			return os.ErrInvalid
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.list {
		if strings.EqualFold(u.list[i].Name, name) {
			u.list[i].PasswordHash = hash
			u.list[i].Role = role
			return u.saveLocked()
		}
	}
	u.list = append(u.list, AuthUser{Name: name, Role: role, PasswordHash: hash})
	return u.saveLocked()
}

// Delete — удалить пользователя по имени (R205).
//
// ЗАЧЕМ. Без удаления нельзя было убрать лишние учётные записи (например, тестовые или
// прежнего администратора после смены имени в .env) — оставался «второй полный доступ».
// Удалить себя нельзя: панель осталась бы без входа.
func (u *UserStore) Delete(name, caller string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return os.ErrInvalid
	}
	if strings.EqualFold(strings.TrimSpace(caller), name) {
		return os.ErrPermission
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.list {
		if !strings.EqualFold(u.list[i].Name, name) {
			continue
		}
		u.list = append(u.list[:i], u.list[i+1:]...)
		return u.saveLocked()
	}
	return os.ErrNotExist
}

// --- сессии ---------------------------------------------------------------

const (
	sessionCookieName = "ol_session"
	sessionTTL        = 12 * time.Hour
	csrfHeaderName    = "X-CSRF-Token"
)

// AuthSession — серверная сессия.
type AuthSession struct {
	ID       string
	User     string
	Role     string
	CSRF     string
	Created  time.Time
	LastSeen time.Time
	// R225: кто и откуда вошёл — оператор видит это в разделе «Пользователи»
	// и может погасить чужую сессию, не меняя пароль.
	IP        string
	UserAgent string
}

// SessionStore — сессии WebUI.
//
// R223 (2026-10-22): СЕССИИ ПЕРЕЖИВАЮТ РЕСТАРТ БАЛАНСЕРА (если задан путь к файлу).
//
// ЖИВОЙ ДЕФЕКТ. R222 убрал вшитый токен: браузер работает только сессией. Но сессии
// лежали в памяти, и любой `docker compose restart loadbalancer` (в том числе
// обновление образа) выбрасывал всех из панели — оператор видел экран входа там,
// где секунду назад была активная работа. Файл кладём на тот же том, что и
// users.json (LB_SESSIONS_FILE, по умолчанию рядом с LB_USERS_FILE), права 0600:
// там cookie-id и CSRF-токены, то есть секреты.
//
// Пишем НЕ на каждый запрос: TTL продлевается скользящим LastSeen, и запись на
// каждое продление — это диск на каждый клик. Ленивая запись: не чаще, чем раз в
// sessionPersistEvery (без таймеров и горутин — по факту изменения).
type SessionStore struct {
	mu          sync.RWMutex
	m           map[string]*AuthSession
	path        string    // "" = только в памяти
	persistedAt time.Time // когда последний раз писали файл
}

// NewSessionStore — хранилище только в памяти (обратная совместимость и тесты).
func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]*AuthSession)}
}

// NewSessionStoreFromFile — хранилище с персистентностью.
//
// Если файла нет, стартуем пустыми (не ошибка). Просроченные сессии при загрузке
// отбрасываются: файл мог пролежать дольше TTL.
func NewSessionStoreFromFile(path string) *SessionStore {
	s := &SessionStore{m: make(map[string]*AuthSession), path: path}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Get().Warnw(i18n.T("api.auth_session.sessions_read_failed"),
				"path", path, "error", err)
		}
		return s
	}
	var file sessionFile
	if err := json.Unmarshal(raw, &file); err != nil {
		logger.Get().Warnw(i18n.T("api.auth_session.sessions_parse_failed"),
			"path", path, "error", err)
		return s
	}
	now := time.Now()
	for _, sess := range file.Sessions {
		if sess == nil || sess.ID == "" || sess.CSRF == "" || sess.User == "" {
			continue
		}
		if now.Sub(sess.LastSeen) > sessionTTL || now.Sub(sess.Created) > sessionTTL {
			continue
		}
		s.m[sess.ID] = sess
	}
	if n := len(s.m); n > 0 {
		logger.Get().Infow(i18n.T("api.auth_session.sessions_restored"),
			"path", path, "sessions", n)
	}
	return s
}

// sessionFile — формат файла сессий.
type sessionFile struct {
	SavedAt  time.Time      `json:"saved_at"`
	Sessions []*AuthSession `json:"sessions"`
}

// sessionPersistEvery — как часто скользящее продление попадает на диск.
const sessionPersistEvery = 5 * time.Minute

// resolveSessionsPath — где лежит файл сессий.
//
// LB_SESSIONS_FILE задаёт путь напрямую. Иначе берём sessions.json рядом с
// users.json: у них общая природа (секреты входа) и общий том, поэтому compose
// не требует новых переменных. Пусто — только память (как было до R223).
func resolveSessionsPath(usersPath string) string {
	if p := strings.TrimSpace(os.Getenv("LB_SESSIONS_FILE")); p != "" {
		return p
	}
	usersPath = strings.TrimSpace(usersPath)
	if usersPath == "" {
		return ""
	}
	dir := filepath.Dir(usersPath)
	if dir == "" || dir == "." {
		return ""
	}
	return filepath.Join(dir, "sessions.json")
}

func randomID(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// Path — путь файла сессий ("" = только в памяти).
func (s *SessionStore) Path() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// saveLocked пишет файл сессий. Вызывать под s.mu (Lock).
//
// Запись атомарная (tmp + rename): иначе рестарт посреди записи оставил бы пустой
// файл, то есть «разлогинил всех» ровно тем действием, ради которого всё это.
func (s *SessionStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	file := sessionFile{SavedAt: time.Now(), Sessions: make([]*AuthSession, 0, len(s.m))}
	for _, sess := range s.m {
		file.Sessions = append(file.Sessions, sess)
	}
	data, err := json.Marshal(&file)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sessions-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // если rename не случился
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	s.persistedAt = time.Now()
	return nil
}

// SessionMeta — откуда пришёл вход (для списка активных сессий).
type SessionMeta struct {
	IP        string
	UserAgent string
}

// Create — новая сессия для пользователя.
func (s *SessionStore) Create(user, role string, meta ...SessionMeta) *AuthSession {
	id := randomID(32)
	csrf := randomID(32)
	if id == "" || csrf == "" {
		return nil
	}
	now := time.Now()
	sess := &AuthSession{ID: id, User: user, Role: role, CSRF: csrf, Created: now, LastSeen: now}
	if len(meta) > 0 {
		sess.IP = meta[0].IP
		sess.UserAgent = meta[0].UserAgent
	}
	s.mu.Lock()
	s.m[id] = sess
	_ = s.saveLocked()
	s.mu.Unlock()
	return sess
}

// Get — сессия по id (с проверкой TTL и скользящим продлением).
func (s *SessionStore) Get(id string) (*AuthSession, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	if time.Since(sess.LastSeen) > sessionTTL {
		delete(s.m, id)
		_ = s.saveLocked()
		return nil, false
	}
	sess.LastSeen = time.Now()
	// Ленивая запись: продлевать TTL на диске на каждый запрос не нужно, но и
	// потерять продление нельзя — иначе после рестарта сессия «стареет» скачком.
	if s.path != "" && time.Since(s.persistedAt) > sessionPersistEvery {
		_ = s.saveLocked()
	}
	return sess, true
}

// Delete — погасить сессию.
func (s *SessionStore) Delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, id)
	_ = s.saveLocked()
	s.mu.Unlock()
}

// DeleteUser — погасить все сессии пользователя (смена пароля).
func (s *SessionStore) DeleteUser(name string) {
	s.mu.Lock()
	for id, sess := range s.m {
		if strings.EqualFold(sess.User, name) {
			delete(s.m, id)
		}
	}
	_ = s.saveLocked()
	s.mu.Unlock()
}

// SessionInfo — запись для списка активных сессий.
//
// ID короткий (8 символов): полный cookie-id — это фактически пароль сессии, и
// отдавать его наружу нельзя даже администратору. Отзыв идёт по префиксу.
type SessionInfo struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`
	Role      string    `json:"role"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen"`
	IP        string    `json:"ip,omitempty"`
	UserAgent string    `json:"user_agent,omitempty"`
}

// List — активные сессии, свежие первыми.
func (s *SessionStore) List() []SessionInfo {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SessionInfo, 0, len(s.m))
	for id, sess := range s.m {
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		out = append(out, SessionInfo{
			ID: shortID, User: sess.User, Role: sess.Role,
			Created: sess.Created, LastSeen: sess.LastSeen,
			IP: sess.IP, UserAgent: sess.UserAgent,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// DeleteByShortID — погасить сессию по префиксу id (то, что видно в UI).
// Неоднозначный префикс считается ошибкой: гасить «не ту» сессию нельзя.
func (s *SessionStore) DeleteByShortID(short string) bool {
	short = strings.TrimSpace(short)
	if s == nil || len(short) < 8 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	match := ""
	for id := range s.m {
		if strings.HasPrefix(id, short) {
			if match != "" {
				return false
			}
			match = id
		}
	}
	if match == "" {
		return false
	}
	delete(s.m, match)
	_ = s.saveLocked()
	return true
}

// Count — число активных сессий (для статуса).
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// requestIsAdmin — админ ли тот, кто пришёл.
//
// AuthMiddleware кладёт в X-Authenticated-Token либо "session:<user>" (браузер,
// роль надо смотреть в сессии), либо сам токен (break-glass = полный доступ).
func requestIsAdmin(r *http.Request, sessions *SessionStore) bool {
	if r == nil {
		return false
	}
	tok := strings.TrimSpace(r.Header.Get("X-Authenticated-Token"))
	if strings.HasPrefix(tok, "session:") {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || sessions == nil {
			return false
		}
		sess, ok := sessions.Get(c.Value)
		return ok && sess.Role == "admin"
	}
	return tok != ""
}

// clientIP — откуда пришёл запрос (за прокси берём первый X-Forwarded-For).
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.Index(v, ","); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// AuthSessionsHandler — GET|DELETE активных сессий (только admin).
//
// Зачем: смена пароля — единственный прежний способ погасить чужой вход, а
// оператору нужно видеть, кто и откуда вошёл, и выкинуть конкретную сессию.
func AuthSessionsHandler(sessions *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sessions == nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "sessions disabled"})
			return
		}
		if !requestIsAdmin(r, sessions) {
			writeAuthJSON(w, http.StatusForbidden, map[string]interface{}{
				"error": i18n.T("api.auth_session.admin_required"),
			})
			return
		}
		switch r.Method {
		case http.MethodGet:
			current := ""
			if c, err := r.Cookie(sessionCookieName); err == nil {
				current = c.Value
			}
			list := sessions.List()
			out := make([]map[string]interface{}, 0, len(list))
			for _, s := range list {
				out = append(out, map[string]interface{}{
					"id": s.ID, "user": s.User, "role": s.Role,
					"created": s.Created, "last_seen": s.LastSeen,
					"ip": s.IP, "user_agent": s.UserAgent,
					"current": current != "" && strings.HasPrefix(current, s.ID),
				})
			}
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{"sessions": out})
		case http.MethodDelete:
			short := strings.TrimSpace(r.URL.Query().Get("id"))
			if short == "" {
				short = strings.TrimPrefix(r.URL.Path, "/api/v1/auth/sessions/")
			}
			if !sessions.DeleteByShortID(short) {
				writeAuthJSON(w, http.StatusNotFound, map[string]interface{}{
					"error": i18n.T("api.auth_session.session_not_found"),
				})
				return
			}
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{"success": true})
		default:
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "method not allowed"})
		}
	}
}

// --- HTTP -----------------------------------------------------------------

func writeAuthJSON(w http.ResponseWriter, status int, body map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// AuthLoginHandler — POST {name,password} → cookie + {user, csrf}.
func AuthLoginHandler(users *UserStore, sessions *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "method not allowed"})
			return
		}
		if users == nil || !users.Enabled() {
			writeAuthJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
				"error": "no users configured: set LB_ADMIN_USER/LB_ADMIN_PASSWORD for the BALANCER " +
					"(not the webui container) or create the file named by LB_USERS_FILE " +
					"(compose default: /app/data/users.json on the balancer data volume)",
			})
			return
		}
		var req struct {
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeAuthJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid request body"})
			return
		}
		usr, ok := users.Verify(req.Name, req.Password)
		if !ok {
			writeAuthJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "invalid credentials"})
			return
		}
		sess := sessions.Create(usr.Name, usr.Role, SessionMeta{
			IP:        clientIP(r),
			UserAgent: strings.TrimSpace(r.UserAgent()),
		})
		if sess == nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "session create failed"})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sess.ID,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   r.TLS != nil,
			MaxAge:   int(sessionTTL / time.Second),
		})
		writeAuthJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"user":    map[string]string{"name": usr.Name, "role": usr.Role},
			"csrf":    sess.CSRF,
		})
	}
}

// AuthLogoutHandler — POST, гасит сессию и cookie.
func AuthLogoutHandler(sessions *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "method not allowed"})
			return
		}
		if c, err := r.Cookie(sessionCookieName); err == nil {
			sessions.Delete(c.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
		writeAuthJSON(w, http.StatusOK, map[string]interface{}{"success": true})
	}
}

// AuthMeHandler — GET, текущий пользователь или authenticated=false.
//
// R225: tokenAuth ставится только для ДЕЙСТВИТЕЛЬНОГО API-токена. До правки
// хватало любого непустого X-API-Token: с неверным токеном панель показывала
// админские действия, а запросы всё равно получали 401.
func AuthMeHandler(users *UserStore, sessions *SessionStore, auth *TokenAuthenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "method not allowed"})
			return
		}
		body := map[string]interface{}{
			"authenticated":   false,
			"usersConfigured": users != nil && users.Enabled(),
		}
		// R270: на изолированном стенде «почему не пускает» должно отвечать API, а
		// не догадки. Отдаём происхождение учётки, путь файла, доступность записи и
		// причину. Пароль и хэш сюда не попадают никогда.
		if users != nil {
			body["bootstrap"] = users.BootstrapInfo()
		}
		if c, err := r.Cookie(sessionCookieName); err == nil {
			if sess, ok := sessions.Get(c.Value); ok {
				body["authenticated"] = true
				body["user"] = map[string]string{"name": sess.User, "role": sess.Role}
				body["csrf"] = sess.CSRF
			}
		}
		// Break-glass: только действительный API-токен даёт админский доступ.
		if tok := strings.TrimSpace(r.Header.Get("X-API-Token")); tok != "" {
			if _, ok := body["user"]; !ok && auth != nil && auth.ValidToken(tok) {
				body["tokenAuth"] = true
			}
		}
		writeAuthJSON(w, http.StatusOK, body)
	}
}

// AuthUsersHandler — GET|PUT (только admin).
func AuthUsersHandler(users *UserStore, sessions *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// R225: раньше роль проверялась только на мутации (AuthMiddleware), и
		// viewer мог читать список пользователей с ролями.
		if !requestIsAdmin(r, sessions) {
			writeAuthJSON(w, http.StatusForbidden, map[string]interface{}{
				"error": i18n.T("api.auth_session.admin_required"),
			})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{"users": users.List()})
		case http.MethodPut:
			var req struct {
				Name     string `json:"name"`
				Password string `json:"password"`
				Role     string `json:"role"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
				writeAuthJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid request body"})
				return
			}
			if err := users.Upsert(req.Name, req.Password, req.Role); err != nil {
				writeAuthJSON(w, http.StatusBadRequest, map[string]interface{}{
					"error": i18n.T("api.auth_session.user_save_failed", err),
				})
				return
			}
			if req.Password != "" {
				sessions.DeleteUser(req.Name)
			}
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{
				"success": true, "users": users.List(),
			})
		case http.MethodDelete:
			name := strings.TrimSpace(r.URL.Query().Get("name"))
			caller := ""
			if c, err := r.Cookie(sessionCookieName); err == nil {
				if sess, ok := sessions.Get(c.Value); ok {
					caller = sess.User
				}
			}
			if err := users.Delete(name, caller); err != nil {
				code := http.StatusBadRequest
				msg := i18n.T("api.auth_session.user_delete_failed", err)
				if err == os.ErrPermission {
					code = http.StatusConflict
					msg = i18n.T("api.auth_session.cannot_delete_self")
				}
				writeAuthJSON(w, code, map[string]interface{}{"error": msg})
				return
			}
			sessions.DeleteUser(name)
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{"success": true, "users": users.List()})
		default:
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "method not allowed"})
		}
	}
}
