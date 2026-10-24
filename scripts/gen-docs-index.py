#!/usr/bin/env python3
# gen-docs-index.py - R233: generate the documentation indexes.
#
# Produces:
#   docs/README.md               - English index with the full table of contents
#   docs/README.ru.md            - Russian index (same structure)
#   docs/archive/README.md    - English index of the historical archive
#   docs/archive/README.ru.md - Russian index of the historical archive
#
# Every current document lives in a category directory and has two language
# variants: <name>.md (English) and <name>.ru.md (Russian). The generator
# detects which variants exist, so the index never lies about translation state.
#
# Usage: python scripts/gen-docs-index.py

import io
import os

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))

# (category, base, path without extension, EN title, RU title, EN description, RU description)
DOCS = [
    ('getting-started', 'installation',
     'getting-started/installation', 'Installation and build',
     'ru=Установка и сборка',
     'Requirements, Docker build, local build, stub mode.',
     'Требования, сборка в Docker, локальная сборка, stub-режим.'),
    ('getting-started', 'deployment',
     'getting-started/deployment', 'Deployment',
     'ru=Развёртывание',
     'Deploying the stack: profiles, workers, environments, ports.',
     'Развёртывание стека: профили, воркеры, окружения, порты.'),
    ('getting-started', 'configuration',
     'getting-started/configuration', 'Configuration',
     'ru=Конфигурация',
     'Balancer config.json, env overrides, nginx and WebUI settings.',
     'config.json балансера, env-переопределения, nginx и настройки WebUI.'),
    ('getting-started', 'hardware-presets',
     'getting-started/hardware-presets', 'Hardware presets',
     'ru=Пресеты под железо',
     'Ready .env presets for RTX 30xx/40xx/50xx and A10.',
     'Готовые .env-пресеты для RTX 30xx/40xx/50xx и A10.'),

    ('architecture', 'balancing-guide',
     'architecture/balancing-guide', 'Balancing guide',
     'ru=Балансировка и сессии',
     'Scoring, backend selection, session and model affinity.',
     'Скоринг, выбор бэкенда, привязка сессий и моделей.'),
    ('architecture', 'backend-type-isolation',
     'architecture/backend-type-isolation', 'Backend type isolation',
     'ru=Изоляция типов бэкендов',
     'How llama.cpp and Ollama backends coexist in one cluster.',
     'Как бэкенды llama.cpp и Ollama живут в одном кластере.'),
    ('architecture', 'rpc-coordinator',
     'architecture/rpc-coordinator', 'RPC coordinator',
     'ru=RPC-координатор',
     'Distributed inference over llama.cpp RPC: roles, state, limits.',
     'Распределённый вывод через llama.cpp RPC: роли, состояние, лимиты.'),
    ('architecture', 'virtual-router',
     'architecture/virtual-router', 'Virtual models',
     'ru=Виртуальные модели',
     'Alias-on-pool virtual models and routing rules.',
     'Виртуальные модели alias-on-pool и правила маршрутизации.'),
    ('architecture', 'layer-sharding',
     'architecture/layer-sharding', 'Layer sharding',
     'ru=Раскладка слоёв',
     'Splitting layers across backends, containers, parameters.',
     'Раскладка слоёв по бэкендам, контейнеры, параметры.'),
    ('architecture', 'rpc-layout-rules',
     'architecture/rpc-layout-rules', 'RPC layout rules',
     'ru=Правила RPC-раскладки',
     'Rules learned from real crashes: same commit, arch, kill switch.',
     'Правила, добытые живыми падениями: один коммит, архитектура, флаг.'),
    ('architecture', 'rpc-link-bandwidth',
     'architecture/rpc-link-bandwidth', 'RPC link bandwidth',
     'ru=Канал между узлами',
     'How link speed between nodes is measured and where it is stored.',
     'Как измеряется скорость канала между узлами и где хранится.'),
    ('architecture', 'bridge-api',
     'architecture/bridge-api', 'C-bridge API',
     'ru=C-bridge API',
     'Go <-> C bridge to llama.cpp: functions, structures, contract.',
     'Мост Go <-> C к llama.cpp: функции, структуры, контракт.'),
    ('architecture', 'balancer-cppworker-contract',
     'architecture/balancer-cppworker-contract', 'Balancer <-> cppworker contract',
     'ru=Контракт балансер <-> cppworker',
     'Wire contract between the balancer and the worker, field by field.',
     'Сетевой контракт балансера и воркера, поле за полем.'),

    ('operations', 'deployment-stack',
     'operations/deployment-stack', 'Deployment stack',
     'ru=Compose-стек',
     'One compose file, profiles, tokens, registry, diagnostics.',
     'Один compose-файл, профили, токены, реестр, диагностика.'),
    ('operations', 'deployment-remote-balancer',
     'operations/deployment-remote-balancer', 'Worker behind a remote balancer',
     'ru=Воркер за удалённым балансером',
     'cppworker + agent on one machine, balancer on another.',
     'cppworker + agent на одной машине, балансер на другой.'),
    ('operations', 'docker-compose-guide',
     'operations/docker-compose-guide', 'Compose files guide',
     'ru=Руководство по compose-файлам',
     'Which compose file is canonical and what the historical ones do.',
     'Какой compose-файл канонический и что делают исторические.'),
    ('operations', 'agent-deployment',
     'operations/agent-deployment', 'Agent deployment',
     'ru=Развёртывание агента',
     'Metrics agent: GPU and CPU modes, systemd, Windows, WSL2.',
     'Агент метрик: режимы GPU и CPU, systemd, Windows, WSL2.'),
    ('operations', 'donor-update',
     'operations/donor-update', 'Donor node update',
     'ru=Обновление донора',
     'Updating and rolling back the donor node image.',
     'Обновление и откат образа узла-донора.'),
    ('operations', 'rpc-donor-vram-lifecycle',
     'operations/rpc-donor-vram-lifecycle', 'Donor VRAM lifecycle',
     'ru=VRAM на узлах раскладки',
     'When VRAM is released on layout nodes and what stays occupied.',
     'Когда освобождается VRAM на узлах раскладки и что остаётся занятым.'),
    ('operations', 'troubleshooting',
     'operations/troubleshooting', 'Troubleshooting',
     'ru=Troubleshooting',
     'Symptoms, causes and fixes collected from live incidents.',
     'Симптомы, причины и исправления из живых инцидентов.'),
    ('operations', 'deployment-troubleshooting',
     'operations/deployment-troubleshooting', 'Deployment incidents',
     'ru=Инциденты развёртывания',
     'Deployment traps and incident write-ups.',
     'Ловушки развёртывания и разборы инцидентов.'),
    ('operations', 'runbook-tools',
     'operations/runbook-tools', 'Tools/tool_calls runbook',
     'ru=Runbook tools/tool_calls',
     'Step-by-step debugging of tool-call resets (scenarios A-G).',
     'Пошаговый дебаг сбросов tool_calls (сценарии A-G).'),

    ('api', 'api',
     'api/api', 'REST API reference',
     'ru=Справочник REST API',
     'All balancer endpoints, auth, WebSocket, Ollama/OpenAI compatibility.',
     'Все эндпоинты балансера, авторизация, WebSocket, совместимость Ollama/OpenAI.'),
    ('api', 'env-vars',
     'api/env-vars', 'Environment variables',
     'ru=Переменные окружения',
     'Single source of truth for every env var, default and effect.',
     'Единый справочник переменных: значение по умолчанию и эффект.'),
    ('api', 'metrics',
     'api/metrics', 'Metrics reference',
     'ru=Справочник метрик',
     'GPU, system, Ollama and proxy metrics with units and meaning.',
     'Метрики GPU, системы, Ollama и прокси: единицы и смысл.'),
    ('api', 'cppworker-metrics-collection',
     'api/cppworker-metrics-collection', 'Worker metrics collection',
     'ru=Сбор метрик воркера',
     'Embedded agent vs external poller: what collects what.',
     'Встроенный агент против внешнего поллера: кто что собирает.'),

    ('features', 'image-generation',
     'features/image-generation', 'Image generation',
     'ru=Генерация изображений',
     'sd.cpp image backend, models, clients, VRAM profiles.',
     'Image-бэкенд sd.cpp, модели, клиенты, профили VRAM.'),
    ('features', 'cppworker-model-params',
     'features/cppworker-model-params', 'Model parameters',
     'ru=Параметры моделей',
     'n_ctx resolver, per-model profiles, RAM fallback, proxy surfaces.',
     'n_ctx resolver, профили моделей, RAM fallback, поверхности прокси.'),
    ('features', 'rpc-benchmark-qwen38',
     'features/rpc-benchmark-qwen38', 'RPC benchmark (27B)',
     'ru=Замер RPC (27B)',
     'Measured layer sharding win on qwen3.8 27B: method and results.',
     'Измеренный выигрыш раскладки на qwen3.8 27B: методика и результаты.'),
    ('features', 'webui-auth',
     'features/webui-auth', 'WebUI auth and roles',
     'ru=Вход и роли WebUI',
     'Sessions, roles, CSRF, break-glass token, nginx proxy.',
     'Сессии, роли, CSRF, аварийный токен, nginx-прокси.'),
    ('features', 'webui-demo',
     'features/webui-demo', 'WebUI demo mode',
     'ru=Демо-режим WebUI',
     'Guest showcase of the whole scenario, enabled by WEBUI_DEMO_MODE only.',
     'Гостевая витрина сценария, включается только WEBUI_DEMO_MODE.'),
    ('features', 'cline-agent-loop',
     'features/cline-agent-loop', 'Cline agent loop',
     'ru=Агентский цикл Cline',
     'Tool-call contract, readiness, tracing and verification.',
     'Контракт tool_calls, ожидание готовности, трассировка, проверка.'),

    ('releases', 'v1.0-release-notes',
     'releases/v1.0-release-notes', 'v1.0 release notes',
     'ru=v1.0: заметки о релизе',
     'What v1.0 contains: working / partial / stubbed.',
     'Что входит в v1.0: работает / частично / заглушка.'),
    ('releases', 'v1.0-rc2-release-notes',
     'releases/v1.0-rc2-release-notes', 'v1.0-rc2 release notes',
     'ru=v1.0-rc2: заметки о релизе',
     'Changes since rc1 and how they were verified.',
     'Изменения с rc1 и как они проверялись.'),
    ('releases', 'v0.7.216-release-notes',
     'releases/v0.7.216-release-notes', 'v0.7.216 release notes',
     'ru=v0.7.216: заметки о релизе',
     'What v0.7.216 contains: demo mode, docs refactor and localisation, log language, CI gates.',
     'v0.7.216: demo-режим, рефакторинг и локализация документации, язык логов, ворота в CI.'),
    ('releases', 'v1.0-smoke-checklist',
     'releases/v1.0-smoke-checklist', 'v1.0 smoke checklist',
     'ru=v1.0: чек-лист smoke',
     'Manual smoke checklist on A10 24 GB before the final tag.',
     'Чек-лист ручного smoke на A10 24 GB перед финальным тегом.'),

    ('development', 'style-compliance',
     'development/style-compliance', 'Style compliance',
     'ru=Стиль и синхронизация документации',
     'Em-dash lint, i18n parity, docs sync rules.',
     'Линт em-dash, паритет i18n, правила синхронизации документации.'),
    ('development', 'i18n-logs',
     'development/i18n-logs', 'Log and error language (R234)',
     'ru=Язык логов и ошибок (R234)',
     'How the log/error language is chosen, switched in the WebUI and synced to workers.',
     'Как выбирается язык логов и ошибок, как '
     'переключается в WebUI и синхронизируется с воркерами.'),
    ('development', 'ci-self-hosted-runner',
     'development/ci-self-hosted-runner', 'Self-hosted CI runner',
     'ru=Self-hosted CI runner',
     'Runner setup, labels, operations and troubleshooting.',
     'Настройка runner-а, лейблы, эксплуатация и диагностика.'),
]

CATEGORY_TITLES = {
    'getting-started': ('Getting started', 'Начало работы'),
    'architecture': ('Architecture', 'Архитектура'),
    'operations': ('Operations', 'Эксплуатация'),
    'api': ('API and reference', 'API и справочники'),
    'features': ('Features', 'Возможности'),
    'releases': ('Releases', 'Релизы'),
    'development': ('Development', 'Разработка'),
}

CATEGORY_INTRO_EN = {
    'getting-started': 'Install, configure and bring the stack up.',
    'architecture': 'How routing, sharding and inference work inside.',
    'operations': 'Run, update and debug a live cluster.',
    'api': 'Endpoint, env and metrics references.',
    'features': 'What the cluster can do for clients.',
    'releases': 'What shipped and how it was checked.',
    'development': 'Conventions and CI for contributors.',
}

CATEGORY_INTRO_RU = {
    'getting-started': 'Установка, настройка и запуск стека.',
    'architecture': 'Как устроены маршрутизация, раскладка и вывод.',
    'operations': 'Эксплуатация, обновление и диагностика живого кластера.',
    'api': 'Справочники по эндпоинтам, переменным и метрикам.',
    'features': 'Что кластер даёт клиентам.',
    'releases': 'Что вошло в релизы и как проверялось.',
    'development': 'Соглашения и CI для участников.',
}

PLAN_INDEX = ('plans/README.md', 'Roadmap and session reports', 'Roadmap и отчёты сессий',
              'Plans, ADR and working session reports (Russian, historical).',
              'Планы, ADR и рабочие отчёты сессий (русский, исторические).')


def exists(rel):
    return os.path.exists(os.path.join(ROOT, 'docs', rel))


def cell(base, lang):
    """Return markdown link or a language marker for one variant."""
    en = base + '.md'
    ru = base + '.ru.md'
    if lang == 'en':
        return '[%s](%s.md)' % ('read', en) if exists(en) else '-'
    return '[читать](%s.ru.md)' % base if exists(ru) else '-'


def row_for(base, lang):
    path = base + ('.md' if lang == 'en' else '.ru.md')
    if exists(path):
        return '[%s](%s)' % ('EN' if lang == 'en' else 'RU', path)
    return '—'


def section_docs(category):
    return [d for d in DOCS if d[0] == category]


def plan_files():
    plans_dir = os.path.join(ROOT, 'docs/plans')
    out = []
    for fn in sorted(os.listdir(plans_dir)):
        p = os.path.join(plans_dir, fn)
        if fn.endswith('.md'):
            out.append(('plans/' + fn, fn[:-3]))
    return out


def write_index(lang):
    ru = lang == 'ru'
    lines = []
    if ru:
        lines.append('# OllamaLegion - документация')
        lines.append('')
        lines.append('> [English](README.md) | **Русский** (текущий)')
        lines.append('>')
        lines.append('> Оглавление всей документации проекта. Один документ - один файл в '
                     'каталоге своей темы.')
        lines.append('> Соглашение об именах: `<имя>.md` - английская версия, '
                     '`<имя>.ru.md` - русская.')
        lines.append('')
        lines.append('Русский язык - исходный для проекта: часть документов пока существует '
                     'только на нём (прочерк `-` в колонке English означает, что перевод '
                     'ещё не готов). Архивные '
                     'материалы (аудиты, отчёты раундов, research-заметки) лежат в '
                     '[`archive/`](archive/README.ru.md) на языке оригинала.')
        lines.append('')
        lines.append('## Содержание')
        lines.append('')
        lines.append('- [Начало работы](#начало-работы)')
        lines.append('- [Архитектура](#архитектура)')
        lines.append('- [Эксплуатация](#эксплуатация)')
        lines.append('- [API и справочники](#api-и-справочники)')
        lines.append('- [Возможности](#возможности)')
        lines.append('- [Релизы](#релизы)')
        lines.append('- [Разработка](#разработка)')
        lines.append('- [Планы и отчёты сессий](#планы-и-отчёты-сессий)')
        lines.append('- [Архив](#архив)')
        lines.append('')
    else:
        lines.append('# OllamaLegion - Documentation')
        lines.append('')
        lines.append('> **English** (current) | [Russian](README.ru.md)')
        lines.append('>')
        lines.append('> Table of contents for the whole documentation set. One document - one '
                     'file in its category directory.')
        lines.append('> Naming: `<name>.md` is English, `<name>.ru.md` is Russian.')
        lines.append('')
        lines.append('Russian is the source language of the project, so a few documents exist '
                     'only in Russian (a dash `-` in the English column means the English '
                     'translation is still pending). Historical material '
                     '(audits, round reports, research notes) lives in '
                     '[`archive/`](archive/README.md) in its original language.')
        lines.append('')
        lines.append('## Contents')
        lines.append('')
        lines.append('- [Getting started](#getting-started)')
        lines.append('- [Architecture](#architecture)')
        lines.append('- [Operations](#operations)')
        lines.append('- [API and reference](#api-and-reference)')
        lines.append('- [Features](#features)')
        lines.append('- [Releases](#releases)')
        lines.append('- [Development](#development)')
        lines.append('- [Plans and session reports](#plans-and-session-reports)')
        lines.append('- [Archive](#archive)')
        lines.append('')

    for cat, (en_title, ru_title) in CATEGORY_TITLES.items():
        docs = section_docs(cat)
        if not docs:
            continue
        title = ru_title if ru else en_title
        anchor = title.lower().replace(' ', '-')
        lines.append('## ' + title)
        lines.append('')
        lines.append(CATEGORY_INTRO_RU[cat] if ru else CATEGORY_INTRO_EN[cat])
        lines.append('')
        lines.append('| Документ | Русский | English | О чём |' if ru
                     else '| Document | English | Russian | What it covers |')
        lines.append('|---|---|---|---|')
        for _cat, _base, path, en_t, ru_t, en_d, ru_d in docs:
            title_cell = ru_t[3:] if ru else en_t
            desc = ru_d if ru else en_d
            if ru:
                lines.append('| **%s** | %s | %s | %s |'
                             % (title_cell, row_for(path, 'ru'), row_for(path, 'en'), desc))
            else:
                lines.append('| **%s** | %s | %s | %s |'
                             % (title_cell, row_for(path, 'en'), row_for(path, 'ru'), desc))
        lines.append('')

    # plans
    plans = plan_files()
    en_t, ru_t = PLAN_INDEX[1], PLAN_INDEX[2]
    title = ru_t if ru else en_t
    lines.append('## ' + title)
    lines.append('')
    lines.append(PLAN_INDEX[4] if ru else PLAN_INDEX[3])
    lines.append('')
    if ru:
        lines.append('- [Roadmap и индекс планов](plans/README.md)')
        for rel, name in plans:
            if rel == 'plans/README.md':
                continue
            lines.append('- [%s](%s)' % (name, rel))
    else:
        lines.append('- [Roadmap and plan index](plans/README.md) (RU)')
        for rel, name in plans:
            if rel == 'plans/README.md':
                continue
            lines.append('- [%s](%s) (RU)' % (name, rel))
    lines.append('')

    # archive
    lines.append('## ' + ('Архив' if ru else 'Archive'))
    lines.append('')
    if ru:
        lines.append('Исторические материалы на языке оригинала: аудиты, research-заметки, '
                     'отчёты раундов, разборы инцидентов, ранние планы. В индекс актуальной '
                     'документации не входят.')
        lines.append('')
        lines.append('Полный перечень: [archive/README.ru.md](archive/README.ru.md)')
    else:
        lines.append('Historical material in its original language: audits, research notes, '
                     'round reports, incident write-ups, early plans. Not part of the current '
                     'documentation set.')
        lines.append('')
        lines.append('Full list: [archive/README.md](archive/README.md)')
    lines.append('')
    lines.append('---')
    lines.append('')
    if ru:
        lines.append('Русский README проекта: [../README.ru.md](../README.ru.md). '
                     'Английский: [../README.md](../README.md).')
    else:
        lines.append('Project README: [../README.md](../README.md). '
                     'Russian: [../README.ru.md](../README.ru.md).')
    lines.append('')
    return '\n'.join(lines)


ARCHIVE_GROUPS = [
    ('audits/', 'Audits', 'Аудиты',
     'Full audits of the wire protocol and client-facing API fidelity.',
     'Полные аудиты сетевого протокола и полноты клиентского API.'),
    ('research/', 'Research', 'Research-заметки',
     'stable-diffusion.cpp on low-VRAM cards, quantization notes.',
     'stable-diffusion.cpp на картах с малой VRAM, заметки о квантовании.'),
    ('rounds/', 'Round reports', 'Отчёты раундов',
     'Design and result reports for individual rounds (R60, Phase 8, Rounds 13-14).',
     'Проектные и итоговые отчёты отдельных раундов (R60, Phase 8, Rounds 13-14).'),
    ('qa/', 'Test and QA plans', 'Тест-планы и QA',
     'Historical bug test plans.',
     'Исторические тест-планы по дефектам.'),
    ('issues/', 'Incident write-ups', 'Разборы инцидентов',
     'Deep dives into specific production incidents.',
     'Подробные разборы конкретных инцидентов.'),
    ('fixes/', 'Fix notes', 'Заметки об исправлениях',
     'Short write-ups of individual bug fixes.',
     'Короткие заметки об отдельных исправлениях.'),
    ('plans/', 'Early plans', 'Ранние планы',
     'Planning documents for work that is already shipped or dropped.',
     'Плановые документы по уже вышедшей или отменённой работе.'),
    ('superpowers/', 'Specs and implementation plans', 'Спеки и планы реализации',
     'Design specs and implementation plans produced with the superpowers workflow.',
     'Дизайн-спеки и планы реализации из процесса superpowers.'),
    ('legacy/', 'Superseded documents', 'Заменённые документы',
     'Older versions of documents that have a current replacement.',
     'Старые версии документов, у которых есть актуальная замена.'),
]


def write_archive_index(lang):
    ru = lang == 'ru'
    lines = []
    if ru:
        lines.append('# Архив документации')
        lines.append('')
        lines.append('> [' + ('English](..' if False else 'English') + '](README.md) | '
                     '**Русский** (текущий)')
        lines.append('')
        lines.append('Исторические материалы, выведенные из актуальной документации при '
                     'рефакторинге (R233). Сохраняются как есть, на языке оригинала: это '
                     'отчёты, аудиты и планы прошлых сессий, а не инструкции.')
        lines.append('')
        lines.append('Актуальная документация: [../README.ru.md](../README.ru.md).')
        lines.append('')
    else:
        lines.append('# Documentation archive')
        lines.append('')
        lines.append('> **English** (current) | [Russian](README.ru.md)')
        lines.append('')
        lines.append('Historical material moved out of the current documentation during the '
                     'R233 refactor. Kept as-is, in its original language: these are reports, '
                     'audits and plans from past sessions, not instructions.')
        lines.append('')
        lines.append('Current documentation: [../README.md](../README.md).')
        lines.append('')
    for dirname, en_t, ru_t, en_d, ru_d in ARCHIVE_GROUPS:
        base = 'archive/' + dirname
        if not exists(base):
            continue
        lines.append('## ' + (ru_t if ru else en_t))
        lines.append('')
        lines.append(ru_d if ru else en_d)
        lines.append('')
        files = []
        for dirpath, _dirnames, filenames in os.walk(os.path.join(ROOT, base)):
            for fn in sorted(filenames):
                if fn.endswith('.md'):
                    rel = os.path.relpath(os.path.join(dirpath, fn), ROOT).replace('\\', '/')
                    files.append(rel)
        for rel in sorted(files):
            name = rel[len('archive/' + dirname):].lstrip('/')
            lines.append('- [%s](%s)' % (name, rel[len('archive/'):] if False else rel))
        lines.append('')
    return '\n'.join(lines)


def main():
    io.open(os.path.join(ROOT, 'docs/README.md'), 'w', encoding='utf-8', newline='\n').write(
        write_index('en'))
    io.open(os.path.join(ROOT, 'docs/README.ru.md'), 'w', encoding='utf-8', newline='\n').write(
        write_index('ru'))
    io.open(os.path.join(ROOT, 'docs/archive/README.md'), 'w', encoding='utf-8',
            newline='\n').write(write_archive_index('en'))
    io.open(os.path.join(ROOT, 'docs/archive/README.ru.md'), 'w', encoding='utf-8',
            newline='\n').write(write_archive_index('ru'))
    print('indexes written: docs/README.md, docs/README.ru.md, docs/archive/README*.md')


if __name__ == '__main__':
    main()

