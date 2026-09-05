# camera-service

RTSP- и ONVIF-фасад для камеры `10.1.0.21` (см. [../CAMERA-PROTOCOL.md](../CAMERA-PROTOCOL.md) и [../CAMERA-10.1.0.21.md](../CAMERA-10.1.0.21.md) в корне репозитория за деталями протокола камеры и историей исследования). Сервис говорит с камерой её собственным протоколом `INF` (TCP/90) и наружу отдаёт обычный RTSP (H.264 over TCP, interleaved) и ONVIF (WS-Discovery + Device/Media/PTZ SOAP).

## Почему сервис не работает на самой камере

Изначально предполагалось запустить этот сервис прямо на камере (см. историю в основном чате/issue). Практическая проверка (трассировка `hiapp` через дизассемблирование + живые тесты) показала: **видео-поток `hiapp` отдаёт только удалённому клиенту** — если подписаться на видео (команда `3` протокола `INF`) с самого устройства (хоть через `127.0.0.1`, хоть через собственный LAN-адрес камеры), `hiapp` сам корректно закрывает соединение сразу после подписки. Не-видео команды (логин, чтение настроек, PTZ) при этом локально работают нормально.

Из-за этого сервис **обязан работать вне камеры** — на Mac, NAS или любом другом всегда включённом хосте той же сети — и подключаться к камере по сети, как обычный удалённый зритель.

## Архитектура

```
cmd/camd/main.go        — точка входа, конфигурация через переменные окружения
internal/inf            — клиент протокола INF: логин, подписка на видео, PTZ (команда 15), разбор кадров
internal/stream         — держит подписку на камеру и раздаёт кадры многим RTSP-клиентам одновременно
internal/rtsp           — RTSP over TCP (interleaved), упаковка H.264 в RTP (RFC 6184, с FU-A для крупных кадров)
internal/onvif          — WS-Discovery + SOAP: Device/Media/PTZ
```

Протокол `INF`, форматы пакетов, коды PTZ и т.д. — см. `CAMERA-PROTOCOL.md`, это не дублируется здесь.

## Конфигурация (переменные окружения)

| Переменная     | По умолчанию      | Назначение |
|----------------|-------------------|------------|
| `CAMERA_HOST`  | `10.1.0.21`       | Адрес камеры |
| `CAMERA_USER`  | `admin`           | Логин для входа по протоколу `INF` |
| `CAMERA_PASSWORD` | ` ` (пусто)    | Пароль для входа по `INF`. Пустое значение = проверенный штатный вход mode 0 (камера вообще не проверяет пароль, см. `CAMERA-PROTOCOL.md` §3). Задавайте, только если вход на этой камере перенастроен на реальную проверку учётных данных (mode 1, тоже подтверждён для `admin`/`admin`) |
| `SERVICE_HOST` | — (обязательна)   | Адрес, по которому **клиенты** увидят этот сервис — используется в RTSP-адресах и ONVIF XAddr/GetStreamUri. Должен быть реально достижим из LAN, автоопределение не выполняется. |
| `RTSP_PORT`    | `554`             | Порт RTSP-сервера |
| `ONVIF_PORT`   | `80`              | Порт ONVIF SOAP (Device/Media/PTZ) |
| `WS_DISCOVERY` | `true`            | Включить WS-Discovery (UDP-мультикаст `239.255.255.250:3702`) |
| `DEVICE_NAME`  | `IPW-F2A2D1E1`    | Значение в ответах `GetDeviceInformation` |

## Запуск

### Docker (основной способ)

```sh
docker compose up --build
```

`docker-compose.yml` по умолчанию использует `network_mode: host` — это нужно, чтобы:
- RTSP (554) и ONVIF (80) были доступны в LAN напрямую по `SERVICE_HOST`;
- WS-Discovery мультикаст реально работал.

**Важно:** Docker Desktop на macOS не поддерживает host-networking так же, как Linux. Если запускаете на macOS — раскомментируйте блок `ports:` в `docker-compose.yml` вместо `network_mode: host`. WS-Discovery мультикаст в этом случае работать не будет (NAT), но RTSP и ONVIF останутся доступны для клиента/NVR, указанного вручную на `SERVICE_HOST`. Предполагаемый целевой хост для постоянной эксплуатации — Linux-машина/NAS в той же сети, а не сам Mac.

### Сборка образа вручную

```sh
docker build -t camera-service:latest .
```

Целевой хост обычно Linux/ARM (NAS, малый одноплатник) — если собираете не на нём самом, нужна кросс-платформенная сборка через `buildx` (одноразовая настройка `docker buildx create --use`, дальше просто переиспользуется):

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t camera-service:latest .   # только локально, без --push собранное не сохраняется для мультиплатформенной сборки
```

### Публикация образа

В registry образ надо запушить под полным именем (`<registry>/<namespace>/<repo>:<tag>`). Два обычных варианта — Docker Hub и GitHub Container Registry (GHCR); шаги одинаковые, отличается только хост реестра и логин.

**Docker Hub:**

```sh
docker login
docker tag camera-service:latest <your-dockerhub-user>/camera-service:latest
docker push <your-dockerhub-user>/camera-service:latest
```

**GHCR (GitHub Container Registry):**

```sh
echo "$GITHUB_TOKEN" | docker login ghcr.io -u <your-github-user> --password-stdin
docker tag camera-service:latest ghcr.io/<your-github-user>/camera-service:latest
docker push ghcr.io/<your-github-user>/camera-service:latest
```

(`GITHUB_TOKEN` — личный access token с правом `write:packages`.)

Для мультиплатформенной публикации сразу в реестр — `buildx` с `--push` вместо отдельных `build`+`tag`+`push`:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/<your-github-user>/camera-service:latest --push .
```

На целевом хосте после публикации — заменить `build: .` на `image:` в `docker-compose.yml`:

```yaml
services:
  camd:
    image: ghcr.io/<your-github-user>/camera-service:latest
    # build: .   # больше не нужно, если образ уже опубликован
```

и запускать `docker compose pull && docker compose up -d` вместо `up --build`.

### Локально, для разработки

```sh
CAMERA_HOST=10.1.0.21 SERVICE_HOST=127.0.0.1 RTSP_PORT=9555 ONVIF_PORT=8080 WS_DISCOVERY=false \
  go run ./cmd/camd
```

Так проверялось при разработке: `ffmpeg -rtsp_transport tcp -i rtsp://127.0.0.1:9555/camera10` и `curl -X POST http://127.0.0.1:8080/onvif/device_service -d '<...GetDeviceInformation.../>'`.

## Что отдаёт сервис

- RTSP: `rtsp://<SERVICE_HOST>:<RTSP_PORT>/camera10` (1920×1080) и `/camera10_sub` (640×360). Только `RTSP over TCP` — транспорт по UDP не поддержан (осознанное упрощение, см. `internal/rtsp`).
- ONVIF SOAP: `http://<SERVICE_HOST>:<ONVIF_PORT>/onvif/{device,media,ptz,imaging}_service`.
- WS-Discovery: отвечает на `Probe` от NVR/ONVIF-клиентов в локальной сети.

## cmd/ptztest

Отдельная CLI-утилита для ручной проверки PTZ напрямую по протоколу `INF`, в обход ONVIF — шлёт одну команду (STOP или короткий импульс с авто-STOP) и выходит. Использовалась для верификации направлений (см. ниже).

```sh
go run ./cmd/ptztest -host 10.1.0.21 -code up -speed 60 -pulse 400ms
```

## cmd/settingstest

CLI для точечного чтения/изменения одного поля внутри `DefaultInfo` (протокол `INF` 100/101), в обход ONVIF. Всегда читает документ заново и переписывает только указанное поле — никогда шаблон целиком.

```sh
go run ./cmd/settingstest -host 10.1.0.21 -field image.mirror            # только прочитать
go run ./cmd/settingstest -host 10.1.0.21 -field audio.first_language -value 3   # прочитать и записать
```

**Важно:** `get` и `set` внутри инструмента используют отдельные TCP-соединения каждый — переиспользование одного соединения для `get` сразу же `set` надёжно давало обрыв (EOF) даже на безопасном no-op.

## Известные ограничения

- **PTZ проверен движением 05.09.2026** (STOP, Up, Down, Left, Right — картинка предсказуемо менялась в нужную сторону; после Up+Down камера точно вернулась в исходный кадр). Диагонали, пресеты и зум не проверялись. Подробности и оговорки — в `CAMERA-PROTOCOL.md` §6.
- **`SetImagingSettings` поддерживает `IrCutFilter` = `AUTO`/`ON`/`OFF`** — все три подтверждены на реальном устройстве. `ON`/`OFF` управляют `adc.polarity` и дают чёткий видимый эффект (цвет/чёрно-белый ИК-режим) даже при ярком дневном свете; переключение занимает ~10–15 сек (механическая задержка ИК-фильтра — не судить по кадру сразу после записи). `AUTO` переключает `adc.day_night_ctrl_type=0`, но **не трогает `polarity`** — если до этого был принудительно выставлен `ON`/`OFF`, собственная авто-логика камеры может со временем это переопределить. Другие поля (`image.mirror`, `led.irlevel`, `led.wlevel`) стабильно обрывают settings-соединение при попытке изменить и не выведены в ONVIF — см. `CAMERA-PROTOCOL.md` §7 за полным списком проверенных/непроверенных полей.
- **Канал настроек (`INF` 100/101) временами нестабилен** независимо от конкретного поля — отдельные `get`/`set` иногда обрываются (EOF/connection reset) без видимой причины. `get`/`set` всегда идут по отдельным TCP-соединениям (переиспользование одного соединения для обоих давало обрыв гораздо чаще), и все вызовы из ONVIF-сервиса (`GetImagingSettings`/`SetImagingSettings`/`GetDeviceInformation`) автоматически повторяются до 3 раз через `inf.WithConn` — на практике этого достаточно.
- **`GetDeviceInformation` отдаёт реальные данные с камеры** (модель, версия прошивки, серийный номер) — разобраны из `DeviceInfo` статическим анализом, см. `CAMERA-PROTOCOL.md` §8.1. При недоступности камеры в момент запроса — откат на `DeviceName` из конфигурации.
- **Без WS-Security/аутентификации.** ONVIF-сервисы открыты для любого в локальной сети — рассчитано на доверенный LAN, как и остальной проект.
- **Аудио не реализовано** — только видео (как и в исходном Mac-мосте `camera_rtsp.py`).
- **Один активный кадр на поток**, а не буфер/DVR — новый RTSP-клиент подключается к текущему живому потоку, перемотка назад не поддерживается.
