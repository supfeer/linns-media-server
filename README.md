# Linn's media server

Минималистичный DLNA-сервер с небольшим desktop-интерфейсом для macOS, Windows и Linux.

## Desktop-приложение

```sh
go run .
```

Добавленные через окно библиотеки сохраняются в системной папке конфигурации. Закрытие окна скрывает приложение в tray; сервер продолжает работать.

## Headless-режим

```sh
go run . -media "/path/to/videos" -name "My Videos"
```

## Локальная установка

```sh
go install fyne.io/tools/cmd/fyne@latest
fyne install
```

## Сборки

GitHub Actions запускает тесты и собирает нативные пакеты для macOS, Windows и Linux. Готовые файлы доступны в artifacts запуска workflow `Build`.

Изменения файлов применяются после двух одинаковых фоновых снимков. Затем сервер увеличивает `SystemUpdateID`, отправляет события `ContentDirectory` и выполняет SSDP `byebye/alive` с прежним UUID.
