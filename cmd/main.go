package main

import (
	"air_avito/internal/app"
	"air_avito/internal/domain"
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ikermy/air_common/pkg/com"
	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_logger/v2/pkg/logger"
)

func main() {
	// Инициализируем инфраструктурные переменные из env vars (порты, домен, TTL, логи).
	// Все значения имеют разумные дефолты; некорректные критичные — fatal.
	mode.InitFromEnv(logger.Fatalf)
	mode.SetTextMode(true)
	mode.SetAudioMode(true)

	// Логгер: режим os.Stdout для Docker
	logSetup := logger.StdOut()
	logSetup.WithLogLevel(logSetup.FromString(mode.GetLogLevel()))
	logSetup.Apply()

	logger.Debug(com.GetVersionInfo())

	// ── Redis ───────────────────────────────────────────────────────────────────
	domain.RedisAddr = os.Getenv("REDIS_ADDR")
	domain.RedisPassword = os.Getenv("REDIS_PASSWORD")
	dbStr := os.Getenv("REDIS_DB")
	if dbStr != "" {
		if n, err := strconv.Atoi(dbStr); err == nil {
			domain.RedisDB = n
		}
	}
	if domain.RedisAddr != "" {
		logger.Info("Redis: адрес=%s, db=%d", domain.RedisAddr, domain.RedisDB)
	} else {
		logger.Warn("Redis: не настроен (REDIS_ADDR пуст)")
	}

	// Корневой контекст процесса, отменяется по сигналам ОС
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	a := app.New(ctx)
	a.Run()

	// Ожидание завершения работы
	<-domain.Exit

	logger.Infoln("Приложение air_avitobot завершено")
}
