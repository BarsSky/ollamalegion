// space.go — сколько места нужно задаче и сколько его есть на целевом томе.
//
// ЗАЧЕМ ЗДЕСЬ, А НЕ ТОЛЬКО В PYTHON. В драйвере проверка уже была, но она
// срабатывает ПОСЛЕ старта: задача перешла в `running`, оператор смотрит на
// прогресс, и только потом приходит «мало места». Проверка до `queued→running`
// отвечает до того, как диск начали трогать, и отвечает числами («нужно X ГиБ,
// свободно Y ГиБ»), а не кодом возврата 4. Python-проверку при этом НЕ убираем:
// между стартом задачи и первой записью место мог занять кто-то ещё, а вторая
// линия обороны стоит один `disk_usage`.
//
// ЧЕГО ЗДЕСЬ НЕТ. Точного знания, сколько займёт результат: конвертер пишет те
// же веса в другую упаковку, поэтому «выход ≈ вход» — оценка, а не обещание.
// Запас 1 ГиБ покрывает шапку результата и служебные файлы ФС; для `convert`
// запас один на два объёма (вход + выход), потому что оба живут одновременно.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// spaceReserveBytes — запас сверх оценки «вход (+ выход)».
//
// ГиБ, а не ГБ: все числа в документах модуля — двоичные (16.6 ГиБ файл),
// и «свободно 1.0 ГиБ» должно означать ровно то, что показывает df.
const spaceReserveBytes int64 = 1 << 30

// spaceNeed — разложенная по слагаемым оценка требуемого объёма.
//
// Слагаемые нужны не для арифметики, а для сообщения: «нужно 33.0 ГиБ» без
// «вход 15.5 + выход 15.5 + запас 1.0» оператору не подсказывает, что делать
// (освободить том или хватит удалить пару старых моделей).
type spaceNeed struct {
	Input  int64 // сколько читаем (файл GGUF или сумма файлов чекпойнта)
	Output int64 // сколько пишем (0 — результат пишется поверх тех же байтов)
	Need   int64 // Input + Output + запас
}

// needForTask считает требуемый объём по цели и источнику.
//
//	normalize (metadata_patch): вход + запас — результат это копия данных входа;
//	convert (hf_to_gguf):      вход + выход + запас, выход ≈ вход;
//	dry_run:                   0 — файл не пишется вообще.
func needForTask(target, src string, opts jobOptions) (spaceNeed, error) {
	if opts.DryRun {
		return spaceNeed{}, nil
	}
	in, err := inputBytes(src)
	if err != nil {
		return spaceNeed{}, err
	}
	need := spaceNeed{Input: in}
	if spec, ok := targetByID(target); ok && spec.Kind == "convert" {
		need.Output = in
	}
	need.Need = need.Input + need.Output + spaceReserveBytes
	return need, nil
}

// inputBytes — объём входа: размер файла или сумма файлов каталога-чекпойнта.
func inputBytes(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !st.IsDir() {
		return st.Size(), nil
	}
	var total int64
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// freeSpaceBytes — свободное место тома, на котором лежит path.
//
// Путь результата может ещё не существовать (каталог создаётся в Create, том —
// нет), поэтому поднимаемся к ближайшему существующему предку: спрашивать
// «место в каталоге, которого нет» бессмысленно, а падать из-за этого нельзя.
func freeSpaceBytes(path string) (int64, error) {
	dir := nearestExistingDir(path)
	if dir == "" {
		return 0, fmt.Errorf("нет существующего каталога в пути %q", path)
	}
	return diskFreeBytes(dir)
}

// nearestExistingDir — ближайший существующий каталог вверх по пути.
func nearestExistingDir(path string) string {
	dir := filepath.Clean(path)
	for {
		if st, err := os.Stat(dir); err == nil {
			if st.IsDir() {
				return dir
			}
			// Файл: спрашиваем про его каталог.
			return filepath.Dir(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// formatGiB — байты в ГиБ с одним знаком: «нужно 33.0 ГиБ, свободно 1.0 ГиБ».
// Один знак после запятой — чтобы 1.04 ГиБ не выглядело как «хватает».
func formatGiB(bytes int64) string {
	return fmt.Sprintf("%.1f ГиБ", float64(bytes)/float64(int64(1)<<30))
}
