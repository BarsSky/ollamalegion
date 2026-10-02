// Command fetch-sdcpp — provisioning-хелпер для image-стенда: скачивает движок
// stable-diffusion.cpp (релиз с GitHub) и модели с HuggingFace.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ИНСТРУМЕНТ: живой E2E (scripts/image-e2e-smoke.ps1) работает на
// моке движка и проверяет протокол, но не доказывает, что РЕАЛЬНЫЙ sd.cpp
// принимает наши запросы и отдаёт картинку. Для этого нужны бинарь движка и
// модель — раньше их приходилось класть руками. Здесь это две команды.
//
// Примеры:
//
//	go run ./tools/fetch-sdcpp list-release leejet/stable-diffusion.cpp win-vulkan
//	go run ./tools/fetch-sdcpp get-release  leejet/stable-diffusion.cpp win-vulkan .\bin\sdcpp
//	go run ./tools/fetch-sdcpp hf-list  second-state/stable-diffusion-v1-5-GGUF
//	go run ./tools/fetch-sdcpp hf-get   second-state/stable-diffusion-v1-5-GGUF sd-v1-5-Q8_0.gguf .\models\image\sd15
//
// Скачивание идёт потоком с прогрессом; для приватных/лимитированных репозиториев
// HuggingFace читается HF_TOKEN (или HUGGING_FACE_HUB_TOKEN).
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const userAgent = "ollamalegion-fetch-sdcpp"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "list-release":
		err = listRelease(args(2, 3))
	case "get-release":
		err = getRelease(args(2, 4))
	case "hf-list":
		err = hfList(args(2, 3))
	case "hf-get":
		err = hfGet(args(2, 5))
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ОШИБКА: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fetch-sdcpp — скачивание движка sd.cpp и моделей

  list-release <owner/repo> [substring]            список ассетов последнего релиза
  get-release  <owner/repo> <substring> <destdir>  скачать и распаковать ассет (zip/tar.gz)
  hf-list      <repo> [substring]                  список файлов репозитория HF
  hf-get       <repo> <filename> <destdir>         скачать файл с HF

Переменные: HF_TOKEN (или HUGGING_FACE_HUB_TOKEN) — токен HuggingFace.
`)
}

// args — позиционные аргументы начиная с индекса from, не более max.
func args(from, max int) []string {
	if len(os.Args) <= from {
		return nil
	}
	out := os.Args[from:]
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// --- GitHub releases --------------------------------------------------------

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease(repo string) (*ghRelease, error) {
	var rel ghRelease
	if err := getJSON("https://api.github.com/repos/"+repo+"/releases/latest", &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func listRelease(a []string) error {
	if len(a) < 1 {
		return fmt.Errorf("нужен <owner/repo>")
	}
	rel, err := fetchLatestRelease(a[0])
	if err != nil {
		return err
	}
	pat := ""
	if len(a) > 1 {
		pat = strings.ToLower(a[1])
	}
	fmt.Printf("релиз %s (%s)\n", a[0], rel.TagName)
	for _, as := range rel.Assets {
		if pat != "" && !strings.Contains(strings.ToLower(as.Name), pat) {
			continue
		}
		fmt.Printf("  %-58s %8.1f MB\n", as.Name, float64(as.Size)/(1<<20))
	}
	return nil
}

func getRelease(a []string) error {
	if len(a) < 3 {
		return fmt.Errorf("нужны <owner/repo> <substring> <destdir>")
	}
	rel, err := fetchLatestRelease(a[0])
	if err != nil {
		return err
	}
	pat := strings.ToLower(a[1])
	dest := a[2]
	var chosen string
	for _, as := range rel.Assets {
		if strings.Contains(strings.ToLower(as.Name), pat) {
			chosen = as.BrowserDownloadURL
			break
		}
	}
	if chosen == "" {
		return fmt.Errorf("в релизе %s нет ассета с %q (см. list-release)", rel.TagName, a[1])
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	archive := filepath.Join(dest, filepath.Base(chosen))
	if err := download(chosen, archive, nil); err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(archive), ".zip") {
		return unzip(archive, dest)
	}
	return fmt.Errorf("распаковка %s не поддерживается (только .zip)", filepath.Base(archive))
}

func unzip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if name == "" || f.FileInfo().IsDir() {
			continue
		}
		out := filepath.Join(dest, name)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode().Perm())
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			w.Close()
			return err
		}
		rc.Close()
		w.Close()
		fmt.Printf("  распакован %s (%.1f MB)\n", name, float64(f.UncompressedSize64)/(1<<20))
	}
	return nil
}

// --- HuggingFace ------------------------------------------------------------

type hfSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}

type hfModel struct {
	ID       string      `json:"id"`
	Siblings []hfSibling `json:"siblings"`
}

func hfList(a []string) error {
	if len(a) < 1 {
		return fmt.Errorf("нужен <repo>")
	}
	var m hfModel
	url := "https://huggingface.co/api/models/" + a[0] + "?blobs=true"
	if err := getJSON(url, &m); err != nil {
		return err
	}
	pat := ""
	if len(a) > 1 {
		pat = strings.ToLower(a[1])
	}
	files := append([]hfSibling(nil), m.Siblings...)
	sort.Slice(files, func(i, j int) bool { return files[i].RFilename < files[j].RFilename })
	fmt.Printf("репозиторий %s\n", m.ID)
	for _, f := range files {
		if pat != "" && !strings.Contains(strings.ToLower(f.RFilename), pat) {
			continue
		}
		fmt.Printf("  %-60s %8.1f MB\n", f.RFilename, float64(f.Size)/(1<<20))
	}
	return nil
}

func hfGet(a []string) error {
	if len(a) < 3 {
		return fmt.Errorf("нужны <repo> <filename> <destdir>")
	}
	url := "https://huggingface.co/" + a[0] + "/resolve/main/" + a[1]
	if err := os.MkdirAll(a[2], 0o755); err != nil {
		return err
	}
	out := filepath.Join(a[2], filepath.Base(a[1]))
	headers := map[string]string{}
	if t := hfToken(); t != "" {
		headers["Authorization"] = "Bearer " + t
	}
	return download(url, out, headers)
}

func hfToken() string {
	for _, k := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// --- HTTP helpers -----------------------------------------------------------

func getJSON(url string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	// GitHub API без токена отдаёт 403 при исчерпании лимита — сообщаем об этом внятно.
	if strings.Contains(url, "api.github.com") {
		if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func download(url, out string, headers map[string]string) error {
	// R-Image: большие файлы (модели — гигабайты) обрываются на стороне HF
	// («stream error: CANCEL; received from peer»). Поэтому качаем с докачкой по
	// Range и повторами: частичный результат лежит в <out>.part и переиспользуется.
	const attempts = 5
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := downloadOnce(url, out, headers); err == nil {
			return nil
		} else {
			lastErr = err
			fmt.Printf("\n  попытка %d/%d не удалась: %v\n", attempt, attempts, err)
			if attempt < attempts {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
			}
		}
	}
	return fmt.Errorf("не удалось скачать после %d попыток: %w", attempts, lastErr)
}

func downloadOnce(url, out string, headers map[string]string) error {
	tmp := out + ".part"

	var offset int64
	if fi, err := os.Stat(tmp); err == nil {
		offset = fi.Size()
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	cl := &http.Client{Timeout: 0} // большие файлы: без общего таймаута, прогресс ниже
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Сервер не поддержал Range — начинаем заново.
		offset = 0
	case http.StatusPartialContent:
		// Докачка: продолжаем с offset.
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return err
	}

	total := resp.ContentLength
	if total > 0 {
		total += offset
	}
	done := offset
	buf := make([]byte, 1<<20)
	start := time.Now()
	lastReport := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			done += int64(n)
			if time.Since(lastReport) > 2*time.Second {
				lastReport = time.Now()
				printProgress(filepath.Base(out), done, total, start)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return fmt.Errorf("чтение потока: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Файл считаем готовым только если размер совпал с ожидаемым (когда он известен).
	if total > 0 && done != total {
		return fmt.Errorf("получено %d из %d байт", done, total)
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	printProgress(filepath.Base(out), done, total, start)
	fmt.Println()
	return nil
}

func printProgress(name string, done, total int64, start time.Time) {
	elapsed := time.Since(start).Seconds()
	speed := float64(done) / (1 << 20) / max(elapsed, 0.001)
	if total > 0 {
		fmt.Printf("\r  %s: %5.1f%% (%.0f/%.0f MB, %.1f MB/s)", name,
			float64(done)*100/float64(total), float64(done)/(1<<20), float64(total)/(1<<20), speed)
	} else {
		fmt.Printf("\r  %s: %.0f MB (%.1f MB/s)", name, float64(done)/(1<<20), speed)
	}
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
