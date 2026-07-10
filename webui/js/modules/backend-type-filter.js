/**
 * backend-type-filter.js — Backend Type Filter & GGUF Tab Visibility
 * OllamaLegion WebUI
 *
 * Управляет видимостью вкладки GGUF и фильтрацией режимов в зависимости
 * от выбранного типа бэкенда (Ollama / llama.cpp).
 */
(function () {
    'use strict';

    const STORAGE_KEY = 'ollamalegion_backend_type';

    /**
     * BackendTypeFilter — центральный модуль фильтрации
     */
    const BackendTypeFilter = {
        /**
         * Флаг защиты от гонки данных: если идёт сохранение на сервер,
         * временные обновления cluster state не должны перезаписывать тип.
         */
        _savingInProgress: false,
        _lastSavedType: null,

        /**
         * Получить текущий тип бэкенда.
         * Приоритет: localStorage > cluster state > API config > fallback (ollama).
         * ВАЖНО: fallback на ollama — только если API config недоступен.
         */
        /**
         * Флаг: была ли уже попытка синхронной загрузки конфига.
         * Защита от множественных синхронных XHR при повторных вызовах.
         */
        _syncAttempted: false,

        getCurrentType: function () {
            // 1. Проверяем кэш localStorage
            var cached = localStorage.getItem(STORAGE_KEY);
            if (cached === 'llama_cpp' || cached === 'ollama') {
                return cached;
            }
            // Round 18g: '' = пользователь явно выбрал "Все". Возвращаем 'all',
            // иначе fallthrough на server config (который может вернуть 'ollama')
            // и active-state в switcher не обновится при выборе "Все".
            if (cached === '') {
                return 'all';
            }

            // 2. Пытаемся получить из API config через кэш (серверный конфиг)
            try {
                var cachedConfig = window.__lastServerConfig;
                if (cachedConfig && cachedConfig.backendEngine) {
                    var bt2 = this._engineToType(cachedConfig.backendEngine);
                    // Сохраняем в localStorage для будущих вызовов
                    localStorage.setItem(STORAGE_KEY, bt2);
                    return bt2;
                }
            } catch (e) { /* ignore */ }

            // 3. ОДИН синхронный запрос к серверу при первой загрузке
            //    (до того как clusterState / async config успели загрузиться)
            if (!this._syncAttempted && window.Api && window.Api.config) {
                this._syncAttempted = true;
                try {
                    var xhr = new XMLHttpRequest();
                    xhr.open('GET', (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE || '') + '/api/v1/config', false);
                    xhr.timeout = 3000;
                    xhr.send();
                    if (xhr.status >= 200 && xhr.status < 300) {
                        var serverConfig = JSON.parse(xhr.responseText);
                        window.__lastServerConfig = serverConfig;
                        if (serverConfig && serverConfig.backendEngine) {
                            var type = this._engineToType(serverConfig.backendEngine);
                            localStorage.setItem(STORAGE_KEY, type);
                            this.updateUI(type);
                            return type;
                        }
                    }
                } catch (e) {
                    console.warn('[BackendTypeFilter] Sync config fetch failed:', e);
                }
            } else {
                // Помечаем что попытка была (даже если API недоступен)
                this._syncAttempted = true;
            }

            // 4. Запускаем асинхронную загрузку для будущих вызовов
            this._fetchConfigAsync();

            // 5. Возвращаем null — UI покажет нейтральное состояние (Автоопределение...)
            //    вместо хардкода 'ollama'. Когда конфиг загрузится асинхронно —
            //    updateUI обновит badge и фильтры.
            return null;
        },

        /**
         * Загрузить конфиг асинхронно и обновить кэш.
         * Вызывается когда нет ни localStorage, ни clusterState.
         */
        _fetchConfigAsync: function () {
            if (!window.Api || !window.Api.config) return;
            var self = this;
            window.Api.config().then(function (serverConfig) {
                window.__lastServerConfig = serverConfig;
                if (serverConfig && serverConfig.backendEngine) {
                    var type = self._engineToType(serverConfig.backendEngine);
                    // Не перезаписываем если уже есть валидное значение в localStorage
                    var existing = localStorage.getItem(STORAGE_KEY);
                    if (existing !== 'llama_cpp' && existing !== 'ollama') {
                        localStorage.setItem(STORAGE_KEY, type);
                        self.updateUI(type);
                    }
                }
            }).catch(function () {
                // API недоступен — остаёмся на fallback
            });
        },

        /**
         * Преобразование backendEngine (серверный формат) в тип (UI формат).
         */
        _engineToType: function (engine) {
            if (engine === 'llama_cpp') return 'llama_cpp';
            if (engine === 'ollama_api') return 'ollama';
            return 'ollama';
        },

        /**
         * Установить тип бэкенда и обновить UI (только локально).
         * Сохранение на сервер происходит при завершении Setup Wizard (buildWizardPayload)
         * или при явном изменении в настройках (settings-ui.js).
         */
        setCurrentType: function (type) {
            // Round 18f: добавили 'all' как 3-й вариант (показывать все бэкенды).
            if (type !== 'ollama' && type !== 'llama_cpp' && type !== 'all') return;
            if (type === 'all') {
                localStorage.setItem(STORAGE_KEY, '');
            } else {
                localStorage.setItem(STORAGE_KEY, type);
            }
            this.updateUI(type);
        },

        /**
         * Сохранить тип бэкенда на сервер через API config.
         * Сервер должен быть источником истины для backendEngine.
         * Включает текущий operatingMode для консистентности конфига,
         * так как toggleModeCards мог автоматически переключить режим на standard
         * если предыдущий был несовместим с новым типом бэкенда.
         * Устанавливает _savingInProgress = true ДО запроса, чтобы заблокировать
         * syncFromClusterState от перезаписи на время сохранения.
         * После сохранения снимает флаг _savingInProgress.
         */
        _saveToServer: function (type) {
            if (!window.Api || !window.Api.updateConfig) {
                this._savingInProgress = false;
                return;
            }
            var self = this;
            // Устанавливаем флаг ДО запроса — это критически важно для защиты от гонки
            self._savingInProgress = true;
            self._lastSavedType = type;
            var payload = {
                backendEngine: type === 'llama_cpp' ? 'llama_cpp' : 'ollama_api'
            };
            // Включаем текущий operatingMode — при смене типа бэкенда
            // toggleModeCards мог переключить режим на standard.
            var modeRadio = document.querySelector('input[name="operatingMode"]:checked');
            if (modeRadio) {
                payload.operatingMode = modeRadio.value;
            }
            window.Api.updateConfig(payload).then(function () {
                // Обновляем кэш серверного конфига
                if (window.__lastServerConfig) {
                    window.__lastServerConfig.backendEngine = type === 'llama_cpp' ? 'llama_cpp' : 'ollama_api';
                    if (modeRadio) {
                        window.__lastServerConfig.operatingMode = modeRadio.value;
                    }
                }
                console.log('[BackendTypeFilter] Successfully saved backend type to server:', type);
            }).catch(function (err) {
                console.warn('[BackendTypeFilter] Failed to save backend type to server:', err);
                // При ошибке — откатываем localStorage к тому что было на сервере
                // (будет пересинхронизировано при следующем cluster state)
            }).finally(function () {
                // Задержка перед снятием флага — даём cluster state время обновиться
                setTimeout(function () {
                    self._savingInProgress = false;
                }, 2000);
            });
        },

        /**
         * Синхронизировать тип из ClusterState API.
         * Пропускает обновление если идёт сохранение на сервер (защита от гонки).
         * Если backendEngine пустой — загружает из конфига API.
         */
        syncFromClusterState: function (state) {
            if (!state) return;

            // Защита от гонки: не перезаписываем тип во время сохранения на сервер
            if (this._savingInProgress) {
                window.__lastClusterState = state;
                return;
            }

            // Защита wizard: не перезаписываем тип из cluster state, пока wizard активен
            if (document.getElementById('setupWizardModal')) {
                window.__lastClusterState = state;
                return;
            }

            // СИНХРОНИЗАЦИЯ: сервер — источник истины для backendEngine.
            // Если _savingInProgress — пропускаем (защита уже сработала выше).
            // Если wizard активен — пропускаем (защита уже сработала выше).
            // В остальных случаях: серверное значение перезаписывает localStorage.
            var localStorageType = localStorage.getItem(STORAGE_KEY);
            var serverEngine = (state.backendEngine || '');
            var effectiveType = state.effectiveBackendType || '';

            console.log('[BackendTypeFilter.syncFromClusterState] localStorage:', localStorageType, 'serverEngine:', serverEngine, 'effectiveBackendType:', effectiveType);

            // Round 18g: пользователь явно выбрал "Все" (localStorage === '').
            // НЕ перезаписываем на serverEngine — иначе выбор "Все" теряется
            // при каждом cluster update (2с цикл) и выделение не перешагивает.
            if (localStorageType === '') {
                window.__lastClusterState = state;
                // UI уже показывает "Все" (getCurrentType возвращает 'all'),
                // updateUI не нужен. Просто return.
                return 'all';
            }

            // Определяем целевой тип из серверных данных
            // Приоритет: effectiveBackendType > backendEngine
            var targetType = null;
            if (effectiveType === 'llama_cpp') {
                targetType = 'llama_cpp';
            } else if (effectiveType === 'ollama') {
                targetType = 'ollama';
            } else if (serverEngine === 'llama_cpp') {
                targetType = 'llama_cpp';
            } else if (serverEngine === 'ollama_api') {
                targetType = 'ollama';
            }

			window.__lastClusterState = state;

			if (targetType) {
				// Сервер — источник истины для backendEngine.
				// ВСЕГДА синхронизируем localStorage и UI из серверных данных.
				// Исключение: если пользовательское значение совпадает с серверным —
				// просто обновляем UI без перезаписи localStorage (избегаем лишних записей).
				if (localStorageType === targetType) {
					console.log('[BackendTypeFilter] localStorage already matches server (' + targetType + '). Updating UI only.');
					this.updateUI(targetType);
					return targetType;
				}
				// Серверное значение отличается от localStorage (или localStorage пуст) —
				// перезаписываем localStorage из сервера
				console.log('[BackendTypeFilter] Server is source of truth. Syncing localStorage from ' + (localStorageType || 'null') + ' to ' + targetType);
				localStorage.setItem(STORAGE_KEY, targetType);
				this.updateUI(targetType);
				return targetType;
			}

            // Сервер не знает тип (backendEngine пуст, effective пуст) —
            // пытаемся получить из API config
            var self = this;
            if (window.Api && window.Api.config) {
                window.Api.config().then(function (serverConfig) {
                    window.__lastServerConfig = serverConfig;
                    if (serverConfig && serverConfig.backendEngine) {
                        var configType = self._engineToType(serverConfig.backendEngine);
                        localStorage.setItem(STORAGE_KEY, configType);
                        self.updateUI(configType);
                    }
                }).catch(function () {
                    // API недоступен — остаёмся на текущем типе из localStorage
                });
            }
        },

        /**
         * Обновить UI: показать/скрыть вкладку GGUF и режимы.
         */
        updateUI: function (type) {
            this.toggleGgufTab(type);
            this.toggleAgentsTab(type);
            this.toggleModeCards(type);
            this.toggleBackendFormFields(type);
            this.updateSettingSections(type);
            this.updateEngineBadge(type);
        },

        /**
         * Обновить маркер типа движка в сайдбаре.
         */
        updateEngineBadge: function (type) {
            var badge = document.getElementById('backendEngineBadge');
            var label = document.getElementById('backendEngineLabel');
            if (!badge || !label) return;

            badge.classList.remove('engine-ollama', 'engine-llama_cpp', 'engine-auto');

            var icon = '🔌';
            var text = (window.I18N ? I18N.t('dashboard.engine_auto') : 'Автоопределение...');
            var cssClass = 'engine-auto';
            var title = (window.I18N ? I18N.t('dashboard.engine_hint_auto') : 'Тип движка: автоопределение');

            if (type === 'llama_cpp') {
                icon = '🦒';
                text = 'llama.cpp';
                cssClass = 'engine-llama_cpp';
                title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
            } else if (type === 'ollama') {
                icon = '🦙';
                text = 'Ollama API';
                cssClass = 'engine-ollama';
                title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
            }

            var iconEl = badge.querySelector('.engine-icon');
            if (iconEl) iconEl.textContent = icon;
            label.textContent = text;
            badge.classList.add(cssClass);
            badge.title = title;

            // Синхронизируем карточки в настройках если они есть
            if (window.SettingsUI && window.SettingsUI.syncBackendEngineCards) {
                window.SettingsUI.syncBackendEngineCards(type);
            }
        },

        /**
         * Показать/скрыть вкладку Agents в сайдбаре.
         * llama.cpp → скрыта (агенты не используются), Ollama → видна.
         */
        toggleAgentsTab: function (type) {
            var agentsNav = document.querySelector('.nav-item[data-page="agents"]');
            if (!agentsNav) return;

            if (type === 'llama_cpp') {
                agentsNav.style.display = 'none';
                // Если текущая страница Agents — переключаем на dashboard
                if (window.ui && window.ui.currentPage === 'agents') {
                    if (typeof window.ui.switchPage === 'function') {
                        window.ui.switchPage('dashboard');
                    }
                }
            } else {
                agentsNav.style.display = '';
                agentsNav.title = '';
            }
        },

        /**
         * Показать/скрыть вкладку GGUF в сайдбаре.
         * llama.cpp → видна, Ollama → скрыта.
         */
        toggleGgufTab: function (type) {
            var ggufNav = document.querySelector('.nav-item[data-page="gguf"]');
            if (!ggufNav) return;

            if (type === 'llama_cpp') {
                ggufNav.style.display = '';
                ggufNav.title = '';
            } else {
                ggufNav.style.display = 'none';
                // Если текущая страница GGUF — переключаем на dashboard
                if (window.ui && window.ui.currentPage === 'gguf') {
                    if (typeof window.ui.switchPage === 'function') {
                        window.ui.switchPage('dashboard');
                    }
                }
            }
        },

        /**
         * Показать/скрыть карточки режимов в настройках.
         * Для Ollama: standard, replication, rpc_coordinator
         * Для llama.cpp: standard, virtual_router, distributed_inference
         */
        toggleModeCards: function (type) {
            var modeCards = document.querySelectorAll('.mode-card');
            modeCards.forEach(function (card) {
                var mode = card.getAttribute('data-mode');
                if (!mode) return;

                var isOllamaOnly = (mode === 'replication' || mode === 'rpc_coordinator');
                var isLlamaCppOnly = (mode === 'virtual_router' || mode === 'distributed_inference');

                if (type === 'ollama' && isLlamaCppOnly) {
                    card.style.display = 'none';
                    // Если выбран скрытый режим — переключаем на standard
                    var radio = card.querySelector('input[type="radio"]');
                    if (radio && radio.checked) {
                        radio.checked = false;
                        var standardRadio = document.querySelector('input[name="operatingMode"][value="standard"]');
                        if (standardRadio) {
                            standardRadio.checked = true;
                            if (window.SettingsUI) {
                                window.SettingsUI.updateModeCards('standard');
                                window.SettingsUI.showModeFields('standard');
                            }
                        }
                    }
                } else if (type === 'llama_cpp' && isOllamaOnly) {
                    card.style.display = 'none';
                    var radio = card.querySelector('input[type="radio"]');
                    if (radio && radio.checked) {
                        radio.checked = false;
                        var standardRadio = document.querySelector('input[name="operatingMode"][value="standard"]');
                        if (standardRadio) {
                            standardRadio.checked = true;
                            if (window.SettingsUI) {
                                window.SettingsUI.updateModeCards('standard');
                                window.SettingsUI.showModeFields('standard');
                            }
                        }
                    }
                } else {
                    card.style.display = '';
                }
            });
        },

        /**
         * Показать/скрыть поля в форме добавления бэкенда.
         */
        toggleBackendFormFields: function (type) {
            var ollamaFields = document.querySelectorAll('.backend-field-ollama');
            var llamaCppFields = document.querySelectorAll('.backend-field-llama');

            ollamaFields.forEach(function (f) {
                f.style.display = (type === 'ollama') ? '' : 'none';
            });
            llamaCppFields.forEach(function (f) {
                f.style.display = (type === 'llama_cpp') ? '' : 'none';
            });
        },

        /**
         * Показать/скрыть секции настроек в зависимости от типа бэкенда.
         */
        updateSettingSections: function (type) {
            // Ollama-специфичные секции
            var ollamaSections = document.querySelectorAll('.settings-section-ollama');
            ollamaSections.forEach(function (s) {
                s.style.display = (type === 'ollama') ? '' : 'none';
            });

            // llama.cpp-специфичные секции
            var llamaCppSections = document.querySelectorAll('.settings-section-llama');
            llamaCppSections.forEach(function (s) {
                s.style.display = (type === 'llama_cpp') ? '' : 'none';
            });
        },

        /**
         * Возвращает список режимов, доступных для данного типа бэкенда.
         */
        getAvailableModes: function (type) {
            if (type === 'ollama') {
                return ['standard', 'replication', 'rpc_coordinator'];
            }
            if (type === 'llama_cpp') {
                return ['standard', 'virtual_router', 'distributed_inference'];
            }
            return ['standard'];
        },

        /**
         * Проверяет, разрешён ли режим для текущего типа бэкенда.
         */
        isModeAvailable: function (mode, type) {
            var modes = this.getAvailableModes(type || this.getCurrentType());
            return modes.indexOf(mode) >= 0;
        },

        /**
         * Фильтрует массив бэкендов по текущему типу.
         * Используется как клиентский fallback, если сервер не отфильтровал.
         * При type='ollama' — оставляет только ollama (или пустой тип как ollama).
         * При type='llama_cpp' — оставляет только llama_cpp.
         * При type=null/undefined — возвращает все без фильтрации.
         * Если после фильтрации результат пуст — возвращает ВСЕ бэкенды (fallback),
         * чтобы UI не оставался пустым при несоответствии типов.
         */
        filterBackends: function (backends, type) {
            if (!backends || !Array.isArray(backends)) return backends || [];
            if (!type) return backends;
            var filtered = backends.filter(function (b) {
                var bt = b.backend_type || b.BackendType || b.type || '';
                if (type === 'ollama') {
                    return bt === 'ollama' || bt === '' || bt === 'ollama_api';
                }
                if (type === 'llama_cpp') {
                    return bt === 'llama_cpp';
                }
                return true;
            });
            // Fallback: если после фильтрации пусто — показываем все (нет ни одного бэкенда нужного типа)
            if (filtered.length === 0) {
                console.debug('[BackendTypeFilter] No backends match type ' + type + '. Showing all backends.');
                return backends;
            }
            return filtered;
        }
    };

    // Экспорт в глобальную область
    window.BackendTypeFilter = BackendTypeFilter;

})();