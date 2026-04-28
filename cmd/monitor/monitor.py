#!/usr/bin/env python3
"""
OllamaLegion Balancer Monitor — real-time TUI утилита для наблюдения
за работой балансировщика нагрузки.

Показывает:
- Загрузку каждого backend (ActiveReqs / MaxConcurrentReqs)
- Состояние очереди (pending + processing)
- Активные сессии
- Куда направляются запросы
- История выбора backend'ов

Запуск:
    python cmd/monitor/monitor.py [--url http://localhost:8080] [--token TOKEN]
"""

import argparse
import curses
import json
import threading
import time
import urllib.request
import urllib.error
from datetime import datetime
from typing import Any, Optional


class BalancerAPI:
    def __init__(self, base_url: str, token: Optional[str] = None):
        self.base_url = base_url.rstrip("/")
        self.token = token

    def _request(self, path: str) -> Optional[Any]:
        url = f"{self.base_url}{path}"
        req = urllib.request.Request(url)
        if self.token:
            req.add_header("X-API-Token", self.token)
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return json.loads(resp.read().decode())
        except Exception:
            return None

    def cluster(self) -> Optional[dict]:
        return self._request("/api/v1/cluster")

    def queue_details(self) -> Optional[dict]:
        return self._request("/api/v1/queue/details")

    def queue_stats(self) -> Optional[dict]:
        return self._request("/api/v1/queue/stats")

    def sessions(self) -> Optional[dict]:
        return self._request("/api/v1/sessions")

    def backends(self) -> Optional[dict]:
        return self._request("/api/v1/backends")


class MonitorApp:
    def __init__(self, api: BalancerAPI, refresh_ms: int = 500):
        self.api = api
        self.refresh_ms = refresh_ms
        self.running = True
        self.data_lock = threading.Lock()

        # Кэш данных
        self.cluster_data: Optional[dict] = None
        self.queue_data: Optional[dict] = None
        self.queue_stats: Optional[dict] = None
        self.sessions_data: Optional[dict] = None
        self.backends_data: Optional[dict] = None

        # История выбора backend'ов (для отслеживания балансировки)
        self.backend_history: list[tuple[str, float]] = []
        self.history_max = 50

        # Ошибки
        self.last_error: Optional[str] = None
        self.last_update: Optional[str] = None

    def _fetch_loop(self):
        while self.running:
            cluster = self.api.cluster()
            queue = self.api.queue_details()
            stats = self.api.queue_stats()
            sessions = self.api.sessions()
            backends = self.api.backends()

            with self.data_lock:
                if cluster:
                    self.cluster_data = cluster
                    self.last_update = datetime.now().strftime("%H:%M:%S")
                    # Обновляем историю backend'ов по ActiveRequests
                    if "backends" in cluster:
                        for b in cluster["backends"]:
                            bid = b.get("id", "")
                            active = b.get("ollama", {}).get("activeRequests", 0)
                            self.backend_history.append((bid, active))
                        # Обрезаем историю
                        while len(self.backend_history) > self.history_max:
                            self.backend_history.pop(0)
                    self.last_error = None
                else:
                    self.last_error = "API недоступен"

                if queue:
                    self.queue_data = queue
                if stats:
                    self.queue_stats = stats
                if sessions:
                    self.sessions_data = sessions
                if backends:
                    self.backends_data = backends

            time.sleep(self.refresh_ms / 1000.0)

    def _color(self, stdscr, ratio: float) -> int:
        """Возвращает цветовую пару в зависимости от загрузки."""
        if ratio < 0.3:
            return curses.color_pair(2)  # зелёный
        elif ratio < 0.6:
            return curses.color_pair(3)  # жёлтый
        else:
            return curses.color_pair(4)  # красный

    def _draw_bar(self, stdscr, y: int, x: int, width: int, ratio: float, label: str):
        """Рисует горизонтальный бар загрузки."""
        filled = int(width * ratio)
        if filled > width:
            filled = width
        bar = "█" * filled + "░" * (width - filled)
        color = self._color(stdscr, ratio)
        stdscr.addstr(y, x, label[:15].ljust(16))
        stdscr.addstr(y, x + 16, f"[{bar}] {ratio*100:5.1f}%", color)

    def run(self, stdscr):
        curses.curs_set(0)
        stdscr.timeout(self.refresh_ms)
        stdscr.clear()

        # Инициализация цветов
        curses.start_color()
        curses.use_default_colors()
        curses.init_pair(1, curses.COLOR_CYAN, -1)    # заголовки
        curses.init_pair(2, curses.COLOR_GREEN, -1)   # низкая загрузка
        curses.init_pair(3, curses.COLOR_YELLOW, -1)  # средняя загрузка
        curses.init_pair(4, curses.COLOR_RED, -1)     # высокая загрузка
        curses.init_pair(5, curses.COLOR_MAGENTA, -1) # акцент

        # Запуск фонового потока polling'а
        thread = threading.Thread(target=self._fetch_loop, daemon=True)
        thread.start()

        while self.running:
            stdscr.clear()
            h, w = stdscr.getmaxyx()

            # Заголовок
            title = " OllamaLegion Balancer Monitor "
            stdscr.addstr(0, (w - len(title)) // 2, title, curses.A_BOLD | curses.color_pair(1))

            # Статус bar
            status = f"URL: {self.api.base_url}  |  Обновление: {self.last_update or '—'}  |  "
            if self.last_error:
                status += f"ОШИБКА: {self.last_error}"
            else:
                status += "OK"
            stdscr.addstr(1, 0, status[:w-1])

            # Линия-разделитель
            stdscr.addstr(2, 0, "─" * (w - 1))

            with self.data_lock:
                self._draw_backends(stdscr, 3, w)
                self._draw_queue(stdscr, 3 + self._backend_rows() + 2, w)
                self._draw_sessions(stdscr, 3 + self._backend_rows() + self._queue_rows() + 4, w)

            stdscr.refresh()

            key = stdscr.getch()
            if key == ord("q") or key == 27:  # q или Esc
                self.running = False

        return 0

    def _backend_rows(self) -> int:
        backends = self.cluster_data.get("backends", []) if self.cluster_data else []
        return max(len(backends) + 3, 4)

    def _queue_rows(self) -> int:
        pending = self.queue_data.get("pending", []) if self.queue_data else []
        processing = self.queue_data.get("processing", []) if self.queue_data else []
        return max(len(pending) + len(processing) + 4, 4)

    def _draw_backends(self, stdscr, start_y: int, width: int):
        """Рисует таблицу бэкендов с загрузкой."""
        stdscr.addstr(start_y, 0, "БЭКЕНДЫ", curses.A_BOLD | curses.color_pair(1))
        stdscr.addstr(start_y + 1, 0, f"{'ID':<12} {'Статус':<10} {'Агент':<6} {'Запросы':<9} {'GPU%':<6} {'VRAM':<10} {'Загрузка'}")
        stdscr.addstr(start_y + 2, 0, "─" * (width - 1))

        if not self.cluster_data:
            stdscr.addstr(start_y + 3, 0, "  нет данных...")
            return

        backends = self.cluster_data.get("backends", [])
        y = start_y + 3
        for b in backends:
            if y >= start_y + self._backend_rows():
                break

            bid = b.get("id", "?")[:12]
            status = b.get("status", "?")[:10]
            has_agent = "да" if b.get("hasAgent") else "нет"
            ollama = b.get("ollama", {})
            active = ollama.get("activeRequests", 0)
            max_req = ollama.get("maxConcurrentRequests", 1)
            if max_req <= 0:
                max_req = 1

            gpu = b.get("gpu", {})
            gpu_pct = gpu.get("usagePercent", 0)
            vram_used = gpu.get("memoryUsed", 0)
            vram_total = gpu.get("memoryTotal", 0)
            vram_str = f"{vram_used}/{vram_total}MB" if vram_total > 0 else "N/A"

            ratio = active / max_req if max_req > 0 else 0

            # Выбираем цвет статуса
            status_color = curses.color_pair(2)
            if status == "unhealthy":
                status_color = curses.color_pair(4)
            elif status == "starting":
                status_color = curses.color_pair(3)

            stdscr.addstr(y, 0, f"{bid:<12}")
            stdscr.addstr(y, 13, f"{status:<10}", status_color)
            stdscr.addstr(y, 24, f"{has_agent:<6}")
            stdscr.addstr(y, 31, f"{active}/{max_req:<7}")
            stdscr.addstr(y, 40, f"{gpu_pct:5.1f}%".ljust(6))
            stdscr.addstr(y, 47, f"{vram_str:<10}")

            bar_width = width - 60
            if bar_width > 10:
                self._draw_bar(stdscr, y, 58, bar_width, ratio, "")

            y += 1

    def _draw_queue(self, stdscr, start_y: int, width: int):
        """Рисует таблицу очереди."""
        stdscr.addstr(start_y, 0, "ОЧЕРЕДЬ", curses.A_BOLD | curses.color_pair(1))

        if self.queue_stats:
            qs = self.queue_stats
            stats_line = (
                f"Workers: {qs.get('workers', '?')}  |  "
                f"Pending: {qs.get('current_size', 0)} / {qs.get('max_size', 0)}  |  "
                f"Processed: {qs.get('processed_total', 0)}"
            )
            stdscr.addstr(start_y + 1, 0, stats_line)

        stdscr.addstr(start_y + 2, 0, f"{'Модель':<25} {'Статус':<12} {'Backend':<12} {'Время ожидания'}")
        stdscr.addstr(start_y + 3, 0, "─" * (width - 1))

        y = start_y + 4
        if self.queue_data:
            pending = self.queue_data.get("pending", [])
            processing = self.queue_data.get("processing", [])
            all_reqs = pending + processing

            for req in all_reqs[:15]:  # показываем первые 15
                if y >= curses.LINES - 1:
                    break

                model = req.get("model", "?")[:24]
                status = req.get("status", "?")[:11]
                target = req.get("target", "?")[:11]
                wait_ms = req.get("waitTimeMs", 0)

                status_color = curses.color_pair(3) if status == "pending" else curses.color_pair(5)

                stdscr.addstr(y, 0, f"{model:<25}")
                stdscr.addstr(y, 26, f"{status:<12}", status_color)
                stdscr.addstr(y, 39, f"{target:<12}")
                stdscr.addstr(y, 52, f"{wait_ms/1000:.1f}s")
                y += 1

        if y == start_y + 4:
            stdscr.addstr(y, 0, "  очередь пуста...")

    def _draw_sessions(self, stdscr, start_y: int, width: int):
        """Рисует активные сессии."""
        stdscr.addstr(start_y, 0, "СЕССИИ", curses.A_BOLD | curses.color_pair(1))
        stdscr.addstr(start_y + 1, 0, f"{'ID':<20} {'Backend':<12} {'Модель':<20} {'Запросов':<10} {'Idle (с)'}")
        stdscr.addstr(start_y + 2, 0, "─" * (width - 1))

        y = start_y + 3
        if self.sessions_data:
            sessions = self.sessions_data.get("sessions", [])
            for sess in sessions[:10]:
                if y >= curses.LINES - 1:
                    break

                sid = sess.get("id", "?")[:19]
                bid = sess.get("backendId", "?")[:11]
                model = sess.get("model", "?")[:19]
                req_count = str(sess.get("requestCount", 0)).ljust(9)
                idle = str(sess.get("idleSeconds", 0)).ljust(8)

                stdscr.addstr(y, 0, f"{sid:<20} {bid:<12} {model:<20} {req_count:<10} {idle}")
                y += 1

        if y == start_y + 3:
            stdscr.addstr(y, 0, "  нет активных сессий...")


def main():
    parser = argparse.ArgumentParser(description="OllamaLegion Balancer Monitor")
    parser.add_argument("--url", default="http://localhost:8080", help="URL балансировщика API")
    parser.add_argument("--token", default=None, help="API токен (если включена аутентификация)")
    parser.add_argument("--refresh", type=int, default=500, help="Интервал обновления, мс (по умолчанию 500)")
    args = parser.parse_args()

    api = BalancerAPI(args.url, args.token)
    app = MonitorApp(api, refresh_ms=args.refresh)

    # Проверка доступности API перед запуском TUI
    print(f"Подключение к {args.url}...")
    test = api.cluster()
    if test is None:
        print(f"ОШИБКА: Не удалось подключиться к {args.url}/api/v1/cluster")
        print("Убедитесь, что балансировщик запущен и API доступен.")
        return 1

    print("Подключено! Запуск монитора (q или Esc для выхода)...")
    time.sleep(0.5)

    return curses.wrapper(app.run)


if __name__ == "__main__":
    exit(main())