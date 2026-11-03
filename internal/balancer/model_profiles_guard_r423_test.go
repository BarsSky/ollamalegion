package balancer

// R423 (2026-11-06): регресс на гонку карты per-model профилей.
//
// CI ловил DATA RACE между SetModelProfile (админка, PUT профиля) и
// GetModelProfile (горячий путь запроса). Тест ниже гоняет ровно эту пару из
// двух горутин: без замка `go test -race` падает, с замком — проходит. Раньше
// гонку ловил случайно тест размещения (TestPlacementP3_AllowDegradedServes_R76),
// то есть регресс мог вернуться незамеченным, если бы тот тест переписали.

import (
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR423_ModelProfilesSnapshotIsCopy — снимок нельзя использовать, чтобы
// незаметно менять config: он копия. Иначе админка, отдавая список профилей в
// JSON, держала бы «живую» карту и обходила замок с другой стороны.
func TestR423_ModelProfilesSnapshotIsCopy(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"m": {ContextLength: 8192},
		},
	}}

	snap := p.ModelProfilesSnapshot()
	if len(snap) != 1 {
		t.Fatalf("в снимке %d профилей, ожидался 1", len(snap))
	}
	snap["m"] = types.LlamaCppModelProfile{ContextLength: 4096}
	snap["other"] = types.LlamaCppModelProfile{ContextLength: 1024}

	if got := p.config.LlamaCppModelProfiles["m"].ContextLength; got != 8192 {
		t.Errorf("правка снимка изменила config: contextLength = %d, ожидалось 8192", got)
	}
	if _, ok := p.config.LlamaCppModelProfiles["other"]; ok {
		t.Error("правка снимка добавила запись в config")
	}
}

// TestR423_UpdateModelProfileKeepsFields — частичное обновление (AutoTune
// per-model) не должно затирать остальные поля профиля: до R423 админка
// читала запись, меняла одно поле и писала назад, и конкурентный PUT профиля
// между чтением и записью терялся целиком.
func TestR423_UpdateModelProfileKeepsFields(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"m": {ContextLength: 16384, Parallel: 2},
		},
	}}

	enabled := true
	p.UpdateModelProfile("m.gguf", func(prof *types.LlamaCppModelProfile) {
		prof.AutoTune = &enabled
	})

	// Ключ нормализуется: `m.gguf` и `m` — одна запись (R136).
	prof, ok := p.GetModelProfile("m")
	if !ok {
		t.Fatal("профиль m пропал после UpdateModelProfile(m.gguf)")
	}
	if prof.AutoTune == nil || !*prof.AutoTune {
		t.Errorf("AutoTune не записался: %+v", prof.AutoTune)
	}
	if prof.ContextLength != 16384 || prof.Parallel != 2 {
		t.Errorf("UpdateModelProfile затёр другие поля: %+v", prof)
	}
}

// TestR423_ConcurrentProfileWritesAndReads — та самая гонка из CI. Под -race
// падает без profilesMu; в обычном прогоне проверяет, что читатели не видят
// «полузаписанных» профилей и ничего не паникует.
func TestR423_ConcurrentProfileWritesAndReads(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"m": {ContextLength: 8192},
		},
	}}

	const rounds = 300
	var wg sync.WaitGroup

	// Писатель: админка сохраняет/снимает профиль (PUT/DELETE).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			p.SetModelProfile("m", types.LlamaCppModelProfile{ContextLength: 8192 + i%2})
			if i%7 == 0 {
				p.DeleteModelProfile("m")
				p.SetModelProfile("m", types.LlamaCppModelProfile{ContextLength: 8192})
			}
		}
	}()

	// Читатели: горячий путь запроса + панель (список профилей).
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				p.GetModelProfile("m")
				p.GetModelProfileNumCtx("m")
				p.profileCapCtx("m", true)
				if n, err := p.CountModelProfiles(); err != nil || n < 0 {
					t.Errorf("CountModelProfiles() = %d, %v", n, err)
					return
				}
				_ = p.ModelProfilesSnapshot()
			}
		}()
	}

	wg.Wait()
}
