// family_generic.go — R268: семейство-фолбэк, то есть ПРЕЖНЕЕ поведение.
//
// ПОЧЕМУ ЭТО ВАЖНО. Не опознанное семейство — не ошибка: до появления пакета
// такие модели обслуживались общим кодом, и ломать их из-за «мы не знаем, что
// это» нельзя. generic регистрируется с минимальным приоритетом и совпадает со
// всем, поэтому Resolve всегда находит хотя бы его; статус при этом Supported
// (диалект неизвестен, но конвертация работает как раньше).
package modelfamily

func init() { Register(genericFamily{genericBase}) }

type genericFamily struct{ base }

func (f genericFamily) Match(string, Meta) bool { return true }

func (f genericFamily) CapabilitiesFor(int) Capabilities {
	return Capabilities{
		NativeTools:      false,
		ReasoningChannel: false,
		ToolCallTextForm: "",
		StopStrings:      defaultStopStrings(),
		// Служебные метки здесь ПУСТЫЕ осознанно: широкий «на всякий случай»
		// список чистит фильтр вызывающего кода, а семейство не должно
		// притворяться, что знает диалект неизвестной модели.
		ServiceMarkers: nil,
	}
}

var genericBase = base{
	id:       GenericID,
	priority: 0,
	versions: VersionRange{},
	tokens:   nil,
	notes:    "фолбэк: диалект не опознан, обслуживание как раньше; отказ не выдаётся",
}
