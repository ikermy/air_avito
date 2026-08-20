package domain

import "time"

const (
	AvitoMessengerV3 = "https://api.avito.ru/messenger/v3"
	// AvitoMessengerV2 AvitoMessengerV1 TODO протестировать использование с AvitoMessengerV3 и удалить!
	AvitoMessengerV2 = "https://api.avito.ru/messenger/v2"
	AvitoMessengerV1 = "https://api.avito.ru/messenger/v1"

	AvitoRespondentTTL   = 1 * time.Hour    // TTL для очистки неактивных респондентов
	AvitoPollingInterval = 60 * time.Second // Интервал опроса непрочитанных чатов (polling)
)
