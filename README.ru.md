# AiR Avito

![air_avito](air_avito_logo.png)

[🇬🇧 English version](README.md)

![Версия Go](https://img.shields.io/badge/Go-1.25.8-00ADD8?logo=go)
![Лицензия](https://img.shields.io/badge/license-MIT-blue)
[![Telegram](https://img.shields.io/badge/Telegram-Join%20Chat-blue?logo=telegram)](https://t.me/marusia_dev)

Микросервис AiR-платформы для подключения Avito Messenger к AI-ассистенту. Сервис авторизует пользователей в Avito, принимает сообщения через webhook, передаёт их в общий контур AiR и отправляет ответы обратно в Avito.

## Защита пользовательских данных

Все пользовательские данные шифруются индивидуальным `MasterKey`. Данный ключ доступен только по паролю пользователя и шифрует API-токен авторизации Авито, историю диалогов, индивидуальные настройки бота. Расшифровка этих данных возможна только после авторизации пользователя в системе индивидуальным паролем пользователя. Даже в случае компрометации или утечки базы данных, все пользовательские данные останутся недоступны как для злоумышленников, так и для администрации сервиса.

## Возможности

- OAuth 2.0-авторизация в Avito и сохранение токенов;
- запуск и остановка персональных Avito-клиентов;
- получение списка чатов и webhook-подписок;
- подписка и отписка от webhook Avito;
- приём событий Avito Messenger;
- передача сообщений AI-маршрутизатору и обработка ответов ассистента;
- интеграция с CRM и операторским режимом;
- восстановление пользовательских клиентов после запуска сервиса;
- Prometheus-метрики и корректное завершение работы.

## Архитектура

```text
Avito Messenger
       |
       | OAuth / REST / webhook
       v
  air_avito (Fiber :8080)
       |            |             |
       v            v             v
     MySQL        Redis       air_orchestrator
                                  (gRPC)
       |
       +--> AI-модели / CRM / операторский контур AiR
```

`air_avito` является HTTP-сервисом интеграции с Avito. Конфигурация и пользовательские мастер-ключи запрашиваются через gRPC-клиент общего сервиса AiR Orchestrator. Пользовательские данные и OAuth-токены хранятся в MySQL; Redis используется для временного состояния первых взаимодействий и не является обязательным — при его недоступности сервис продолжает работу с ограниченной функциональностью восстановления этого состояния.

## HTTP API

HTTP-сервер слушает порт `8080`.

```text
GET  /metrics

GET  /v1/avito/status?uid={uid}
POST /v1/avito/auth/url?uid={uid}
GET  /v1/avito/enable?uid={uid}
GET  /v1/avito/disable?uid={uid}
GET  /v1/avito/chats?uid={uid}&limit={limit}&offset={offset}&unread_only={bool}
GET  /v1/avito/subscriptions?uid={uid}
POST /v1/avito/subscribe?uid={uid}
POST /v1/avito/unsubscribe?uid={uid}&url={webhook_url}

GET  /open/avito/available
GET  /open/avito/auth/callback?code={code}&state={state}
POST /open/avito/webhook
```

Запрос к `/v1/avito/auth/url` принимает JSON:

```json
{
  "url": "example.com",
  "client_id": "avito-client-id",
  "client_secret": "avito-client-secret"
}
```

Публичные маршруты `/open/avito/*` используются Avito для проверки доступности, OAuth callback и webhook. Маршруты `/v1/avito/*` требуют обязательный query-параметр `uid`.

Полное описание API находится в [`doc/openapi.yaml`](doc/openapi.yaml).

## Технологии

- Go 1.25.8;
- Fiber v3 — HTTP-сервер и маршрутизация;
- MySQL — постоянное хранение данных и токенов Avito;
- Redis — временный кэш first-interaction;
- gRPC — связь с `air_orchestrator`;
- OAuth 2.0 и REST API Avito Messenger;
- Prometheus — метрики;
- Docker / Docker Compose — запуск и развёртывание;
- Loki — сбор контейнерных логов;
- OpenAI, Google и Mistral — маршруты AI-моделей через `air_common`.

## Запуск

Для development используется [`dev.yml`](dev.yml):

```bash
docker compose -f dev.yml up --build
```

Production-конфигурация находится в [`prod.yml`](prod.yml):

```bash
docker compose -f prod.yml up -d
```

Сервис подключается к внешним Docker-сетям `air_shared` и `monitoring_shared`. MySQL и Redis должны быть доступны в сети под именами `air_db` и `air_redis`, а gRPC-сервис конфигурации — по адресу `airorc:50051`.

## Конфигурация

Основные переменные окружения:

```text
DB_HOST=air_db:3306
DB_NAME=air
DB_USER
DB_PASSWORD
REDIS_ADDR=air_redis:6379
REDIS_PASSWORD
REDIS_DB=0
GRPC_CONFIG_HOST=airorc:50051
SERVICE_KEY_FILE=/run/secrets/service_key
REAL_URL
LOG_LEVEL=info
GLOB_USER_MODEL_TTL=1440
```

`SERVICE_KEY_FILE` указывает на смонтированный read-only файл сервисного ключа. Публичный адрес для OAuth redirect URI задаётся через `REAL_URL` (в production — переменной `DOMAIN`, подставляемой в `prod.yml`).

## Связанные сервисы

- [air_common](https://github.com/ikermy/air_common) — Общая библиотека для AI‑микросервисов
- [air_orchestrator](https://github.com/ikermy/air_orchestrator) — Главный сервис оркестратор
- [air_operator](https://github.com/ikermy/air_operator) — Сервис переадресации ответов на операторов от пользователей, поддерживает все типы ботов
- [marusia_crm](https://github.com/ikermy/marusia_crm) — Сервис интеграции с внешними CRM системами
- [air_logger](https://github.com/ikermy/air_logger) — Вспомогательный сервис логирования событий с поддержкой многопользовательского режима и поддержкой сборщика логов loki

## Лицензия

Проект распространяется по лицензии [MIT](LICENSE). Она разрешает свободно использовать, копировать, изменять и распространять программное обеспечение при сохранении текста лицензии и уведомления об авторских правах.

Полный текст лицензии доступен в файле [`LICENSE`](LICENSE).

## Контакты
[![Telegram](https://img.shields.io/badge/Telegram-Contact-blue?logo=telegram)](https://t.me/ikermy)

