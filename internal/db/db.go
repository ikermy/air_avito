package db

import (
	"air_avito/internal/domain"
	"air_avito/internal/repository"
	"air_avito/internal/repository/mysql"
	"context"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/ikermy/air-common/pkg/comdb"
	"github.com/ikermy/air-common/pkg/comdom"
	"github.com/ikermy/air-logger/v2/pkg/logger"
)

// UserDetails представляет информацию о пользователе, включая настройки телеграм-бота
type UserDetails struct {
	UserId       int64               // Идентификатор пользователя
	Avito        string              // JSON с OAuth токенами Avito
	AvitoEnabled bool                // Флаг включения Avito интеграции
	AssistName   string              // Имя ассистента
	AssistantId  string              // Идентификатор ассистента
	Provider     comdom.ProviderType // Тип провайдера: 1=OpenAI, 2=Mistral
	MetaAction   string              // Поле MetaAction из модели ассистента
	Triggers     []string            // Список триггеров из модели ассистента
	Espero       uint8               // Значение Espero
	AskLimit     uint32              // Лимит запросов
	Ignore       bool                // Игнорировать сообщения до ответа ассистента
	Events       Notifications       // При каких событиях присылать уведомления
}

// AvitoToken представляет OAuth токены для Avito (хранится в JSON в channels.Avito)
type AvitoToken struct {
	AvitoUserID       string    `json:"avito_user_id"` // ID аккаунта Avito пользователя (например: "430008552")
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	TokenType         string    `json:"token_type"`
	Expiry            time.Time `json:"expiry"`
	Scopes            []string  `json:"scopes"`
	ClientID          string    `json:"client_id"`           // Персональный Avito Client ID пользователя
	ClientSecret      string    `json:"client_secret"`       // Персональный Avito Client Secret пользователя
	RedirectUrlPrefix string    `json:"redirect_url_prefix"` // Персональный URL префикс для callback/webhook (например: "strapless-alanna-quietly.ngrok-free.dev")
}

// Notifications события уведомлений
type Notifications struct {
	Start  bool
	End    bool
	Target bool
}

// NullBytes Промежуточный тип для загрузки массива байт из базы
type NullBytes struct {
	Bytes []byte
	Valid bool // Valid = true, если Bytes не NULL
}

type DB struct {
	*comdb.DB
	repo repository.Repository
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
		DB:   base,
		repo: repo,
	}, nil
}

func (d *DB) HandlerClose() {
	go func() {
		// Получаю сигнал о завершении работы от главного контекста приложения
		<-d.MainCTX().Done()
		logger.Info("DB: контекст отменен, ожидаю завершения всех операций...")

		// Ожидаем сигнал о завершении от компонентов работающих с ДБ
		<-domain.UsersDB
		logger.Info("DB: все модули работающие с БД завершили работу, продолжаю остановку...")

		if err := d.Close(); err != nil {
			logger.Error("DB: ошибка при закрытии: %v", err)
		}

		close(domain.Exit)
	}()
}
