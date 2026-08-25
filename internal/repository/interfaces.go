package repository

import (
	"air_avito/internal/domain"
	"context"
	"time"

	"github.com/ikermy/air-common/pkg/comdb"
)

// Interior описывает внутренние методы доступа к данным приложения.
type Interior interface {
	GetAvitoToken(ctx context.Context, userID uint32, mk [32]byte) (*domain.Token, error)
	SaveAvitoToken(ctx context.Context, userID uint32, mk [32]byte, token domain.Token) error
	UpdateAvitoToken(ctx context.Context, userID uint32, mk [32]byte, accessToken string, expiry time.Time) error
	GetAvitoUsers(ctx context.Context) ([]domain.UserDetails, error)
	GetAvitoUser(ctx context.Context, userID uint32) (*domain.UserDetails, error)
}

// Exterior описывает общие внешние DB-методы.
type Exterior interface {
	comdb.Exterior
}

// Repository агрегирует внутренние и внешние контракты доступа к данным.
type Repository struct {
	Internal Interior
	External Exterior
}
