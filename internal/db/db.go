package db

import (
	"air_avito/internal/domain"
	"air_avito/internal/repository"
	"air_avito/internal/repository/mysql"
	"context"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/ikermy/air-common/pkg/comdb"
	"github.com/ikermy/air-logger/v2/pkg/logger"
)

// NullBytes Промежуточный тип для загрузки массива байт из базы
type NullBytes struct {
	Bytes []byte
	Valid bool // Valid = true, если Bytes не NULL
}

type DB struct {
	*comdb.DB
	repo repository.Repository

	done   sync.Once     // На всякий случай однократное закрытие канала
	DoneCh chan struct{} // Канал уведомления о завершении операций пользователями ДБ
	Exit   chan struct{} // Канал завершения работы приложения
}

func (d *DB) GetAvitoToken(ctx context.Context, userID uint32, mk [32]byte) (*domain.Token, error) {
	return d.repo.Internal.GetAvitoToken(ctx, userID, mk)
}

func (d *DB) SaveAvitoToken(ctx context.Context, userID uint32, mk [32]byte, token domain.Token) error {
	return d.repo.Internal.SaveAvitoToken(ctx, userID, mk, token)
}

func (d *DB) UpdateAvitoToken(ctx context.Context, userID uint32, mk [32]byte, accessToken string, expiry time.Time) error {
	return d.repo.Internal.UpdateAvitoToken(ctx, userID, mk, accessToken, expiry)
}

func (d *DB) GetAvitoUsers(ctx context.Context) ([]domain.UserDetails, error) {
	return d.repo.Internal.GetAvitoUsers(ctx)
}

func (d *DB) GetAvitoUser(ctx context.Context, userID uint32) (*domain.UserDetails, error) {
	return d.repo.Internal.GetAvitoUser(ctx, userID)
}

// New создаёт подключение к БД и инициализирует репозитории
func New(parent context.Context) (*DB, error) {
	base, err := comdb.New(parent)
	if err != nil {
		return nil, err
	}
	repo, err := mysql.New(base)
	if err != nil {
		return nil, err
	}
	return &DB{
		DB:     base,
		repo:   repo,
		DoneCh: make(chan struct{}),
		Exit:   make(chan struct{}),
	}, nil
}

func (d *DB) HandlerClose() {
	go func() {
		// Получаю сигнал о завершении работы от главного контекста приложения
		<-d.MainCTX().Done()
		logger.Info("DB: контекст отменен, ожидаю завершения всех операций...")

		// Ожидаем сигнал о завершении от компонентов работающих с ДБ
		<-d.DoneCh
		logger.Info("DB: все модули работающие с БД завершили работу, продолжаю остановку...")

		if err := d.Close(); err != nil {
			logger.Error("DB: ошибка при закрытии: %v", err)
		}

		close(d.Exit)
	}()
}

func (d *DB) CloseDoneCh() {
	d.done.Do(func() {
		close(d.DoneCh)
	})
}

func (d *DB) GetExitCh() <-chan struct{} {
	return d.Exit
}
