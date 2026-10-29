// donor_role_r304_test.go — R304 (2026-10-30): тесты единого предиката донора.
//
// ЧТО ПРОВЕРЯЕТСЯ. Правило «эта машина — донор, значит своих копий моделей не
// держит» — единственное место, от которого зависят пять путей загрузки и отчёт
// /api/models. Ошибка здесь не «мелкий баг», а либо простой всего стенда (все
// машины разом перестали грузить), либо донор, который отнял у мастера память.
// Поэтому проверяются и приказ, и факт узла, и границы (nil-бэкенд, неизвестная
// роль).
package cppbackend

import "testing"

// TestR304DONOR_DonorMarkTable — таблица правил донорства.
//
// ПОРЯДОК ПРАВИЛ ЗАФИКСИРОВАН ТЕСТОМ (role=donor старше узла). Это не
// придирка: причина отказа уезжает оператору и в панель, и «донор потому что
// включён узел» там, где роль объявлена приказом, сбивало бы разбор.
func TestR304DONOR_DonorMarkTable(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		nodeUp     bool
		wantDonor  bool
		wantReason string
		why        string
	}{
		{
			name: "роль donor",
			role: RoleDonor,
			// Узел ещё не поднялся — роль всё равно решает: окно между приказом
			// и стартом процесса не должно разрешать загрузку.
			nodeUp:     false,
			wantDonor:  true,
			wantReason: DonorReasonRole,
			why:        "приказ балансера старше любого факта состояния процесса",
		},
		{
			name:       "role=main с поднятым узлом",
			role:       RoleMain,
			nodeUp:     true,
			wantDonor:  true,
			wantReason: DonorReasonRPCServerEnabled,
			why:        "машина отдаёт память мастеру — своих копий не поднимает",
		},
		{
			name:       "роль неизвестна, узла нет",
			role:       "",
			nodeUp:     false,
			wantDonor:  false,
			wantReason: "",
			why:        "нет ни приказа, ни узла — запрещать загрузку по догадке нельзя",
		},
		{
			name: "role=standalone с поднятым узлом",
			role: RoleStandalone,
			// НАМЕРЕННОЕ РЕШЕНИЕ (R304): роль standalone НЕ отменяет донорства по
			// факту узла. Узел включает либо балансер (только у donor), либо
			// оператор руками — и во втором случае машина уже донор: она принимает
			// соединения мастера и отдаёт свою память. Позволить ей одновременно
			// грузить своё значит дважды продать одну и ту же память.
			nodeUp:     true,
			wantDonor:  true,
			wantReason: DonorReasonRPCServerEnabled,
			why:        "факт поднятого узла делает машину донором независимо от роли",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Backend{cfg: Config{BackendRole: c.role, RPCServerEnabled: c.nodeUp}}

			mark := b.DonorMark()
			if mark.Role != c.role {
				t.Errorf("mark.Role = %q, want %q", mark.Role, c.role)
			}
			if mark.Donor != c.wantDonor {
				t.Errorf("mark.Donor = %v, want %v (%s)", mark.Donor, c.wantDonor, c.why)
			}
			if mark.CanHostOwnModels == c.wantDonor {
				t.Errorf("mark.CanHostOwnModels = %v при Donor=%v — пометка сама себе противоречит",
					mark.CanHostOwnModels, mark.Donor)
			}
			if mark.Reason != c.wantReason {
				t.Errorf("mark.Reason = %q, want %q (%s)", mark.Reason, c.wantReason, c.why)
			}

			donor, reason := b.DonorOnly()
			if donor != c.wantDonor || reason != c.wantReason {
				t.Errorf("DonorOnly() = (%v, %q), want (%v, %q)",
					donor, reason, c.wantDonor, c.wantReason)
			}
		})
	}
}

// TestR304DONOR_OwnCopiesOnlyForDonor — список «вопиющих» копий.
//
// ПОЧЕМУ ТОЛЬКО У ДОНОРА. У не-донора загруженная модель роли не противоречит:
// если показывать её как «копию вопреки роли», панель кричала бы о норме.
func TestR304DONOR_OwnCopiesOnlyForDonor(t *testing.T) {
	models := map[string]*modelInstance{
		"loaded-b":  {info: ModelInfo{Name: "loaded-b", State: StateLoaded}},
		"loading-a": {info: ModelInfo{Name: "loading-a", State: StateLoading}},
		// Выгруженная и «в ошибке» — НЕ копии: памяти они не держат.
		"unloaded-c": {info: ModelInfo{Name: "unloaded-c", State: StateUnloaded}},
		"error-d":    {info: ModelInfo{Name: "error-d", State: StateError}},
	}

	donor := &Backend{cfg: Config{BackendRole: RoleDonor}, models: models}
	mark := donor.DonorMark()
	if mark.OwnCopies == nil {
		t.Fatal("у донора с загруженной моделью OwnCopies пуст — панель не увидит, что нужно выгрузить")
	}
	if len(mark.OwnCopies) != 2 {
		t.Fatalf("OwnCopies = %v, want 2 модели (loaded + loading)", mark.OwnCopies)
	}
	// Порядок отсортирован: снимок сравнивается между опросами, а обход map в
	// ListModels случаен — без сортировки поле «дрожало» бы на пустом месте.
	if mark.OwnCopies[0] != "loaded-b" || mark.OwnCopies[1] != "loading-a" {
		t.Errorf("OwnCopies = %v, want [loaded-b loading-a] (отсортировано)", mark.OwnCopies)
	}

	notDonor := &Backend{cfg: Config{}, models: models}
	if m := notDonor.DonorMark(); len(m.OwnCopies) != 0 {
		t.Errorf("у не-донора OwnCopies = %v, want пусто (копия роли не противоречит)", m.OwnCopies)
	}
}

// TestR304DONOR_NilBackendIsNotDonor — nil не паникует и не запрещает.
//
// ЗАЧЕМ. Пометку читают обработчики на самом раннем старте и тестовые стенды без
// поднятого бэкенда. Паника здесь означала бы «воркер не отвечает вовсе» вместо
// «загрузка прошла как раньше».
func TestR304DONOR_NilBackendIsNotDonor(t *testing.T) {
	var b *Backend
	if donor, reason := b.DonorOnly(); donor || reason != "" {
		t.Errorf("DonorOnly(nil) = (%v, %q), want (false, \"\")", donor, reason)
	}
	mark := b.DonorMark()
	if mark.Donor || !mark.CanHostOwnModels {
		t.Errorf("DonorMark(nil) = %+v, want Donor=false, CanHostOwnModels=true", mark)
	}
}

// TestR304DONOR_SetBackendRoleAndNode — донесение приказа до ЖИВОГО конфига.
//
// ЗАЧЕМ ОТДЕЛЬНО. `PUT /config/update` пишет в currentConfig пакета cppworker, а
// решения принимает b.cfg: без этих сеттеров роль и факт узла оставались бы в
// ответе API, не меняя поведения (ровно так уже терялся rpcEnabled — см.
// SetRPCConfig).
func TestR304DONOR_SetBackendRoleAndNode(t *testing.T) {
	b := &Backend{cfg: Config{}}

	// Роль приходит с регистром и пробелами — нормализуется.
	b.SetBackendRole(" Donor ")
	if got := b.Config().BackendRole; got != RoleDonor {
		t.Fatalf("после SetBackendRole(\" Donor \") роль = %q, want %q", got, RoleDonor)
	}
	if donor, reason := b.DonorOnly(); !donor || reason != DonorReasonRole {
		t.Fatalf("DonorOnly() = (%v, %q), want (true, %q)", donor, reason, DonorReasonRole)
	}

	// Снятие роли + поднятый узел: машина остаётся донором по факту узла.
	b.SetBackendRole(RoleStandalone)
	b.SetRPCServerEnabled(true)
	if donor, reason := b.DonorOnly(); !donor || reason != DonorReasonRPCServerEnabled {
		t.Fatalf("role=standalone + узел: DonorOnly() = (%v, %q), want (true, %q)",
			donor, reason, DonorReasonRPCServerEnabled)
	}

	// Узел погашен — запрета нет.
	b.SetRPCServerEnabled(false)
	if donor, reason := b.DonorOnly(); donor || reason != "" {
		t.Errorf("после выключения узла DonorOnly() = (%v, %q), want (false, \"\")", donor, reason)
	}
}

// TestR304DONOR_NormalizeBackendRole — какие значения ролью считаются.
//
// Пустая строка — ЛЕГАЛЬНОЕ значение («роль не объявлена»), а не ошибка: так
// балансер снимает роль, и это не должно попадать в validation_errors.
func TestR304DONOR_NormalizeBackendRole(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		known bool
	}{
		{"", "", true},
		{"donor", RoleDonor, true},
		{"DONOR", RoleDonor, true},
		{"  main ", RoleMain, true},
		{"standalone", RoleStandalone, true},
		{"donnor", "", false},
		{"dono r", "", false},
	}
	for _, c := range cases {
		got, known := NormalizeBackendRole(c.in)
		if known != c.known || got != c.want {
			t.Errorf("NormalizeBackendRole(%q) = (%q, %v), want (%q, %v)",
				c.in, got, known, c.want, c.known)
		}
	}
}
