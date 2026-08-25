package avito

import (
	"air_avito/internal/repository"
	"context"
	"sync"
	"time"

	"github.com/ikermy/air-common/pkg/crm"
	"github.com/ikermy/air-common/pkg/endpoint"
	"github.com/ikermy/air-common/pkg/model"
)

// DB Интерфейсы для зависимостей
type DB interface {
	ExtDB
	IntDB
}

type Model = model.Inter
type Endpoint = endpoint.Inter
type ExtDB = repository.Exterior
type IntDB = repository.Interior
type CRM = crm.Inter

// Operator представляет интерфейс для работы с оператором
type Operator interface {
	AskOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) (model.Message, error)
	SendToOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) error
	ReceiveFromOperator(ctx context.Context, userID uint32, dialogID uint64) <-chan model.Message
	DeleteSession(userID uint32, dialogID uint64) error
	GetConnectionErrors(ctx context.Context, userID uint32, dialogID uint64) <-chan string
	CloseOperatorSSE(ctx context.Context, userID uint32, dialogID uint64) error
}

type ORCClient interface {
	GetUserMasterKey(ctx context.Context, userId uint32) ([32]byte, error)
}

// RespondentData хранит данные о респонденте для идемпотентности
type RespondentData struct {
	LastMessageID string    // ID последнего обработанного сообщения
	LastSeen      time.Time // Время последней активности
	ChatID        string    // ID чата в Avito
	AuthorID      int64     // ID автора в Avito
	AuthorName    string    // Имя автора (nickname)
}

// Client представляет клиент Avito для конкретного пользователя
type Client struct {
	ctx               context.Context
	cancel            context.CancelFunc
	clientID          string
	clientSecret      string
	redirectUrlPrefix string // Персональный URL префикс пользователя
	userID            uint32 // ID пользователя-владельца
	db                DB
	mod               Model
	end               Endpoint
	mk                [32]byte // Ключ пользователя для шифрования данных в БД
	crm               *crm.User
	assist            *model.Assistant
	respondents       sync.Map // key: respId (uint64), value: *domainavito.RespondentData
	knownUsers        sync.Map // key: respId (uint64), value: bool
	// Редиско кеширование первого взаимодействия
	redisCache CacheMethods // при первоначальной загрузке тащит первые контакты из redis
}

// User управляет всеми клиентами Avito
type User struct {
	ctx          context.Context
	cancel       context.CancelFunc
	clientID     string
	clientSecret string
	db           DB
	mod          Model
	end          Endpoint
	crm          CRM
	op           Operator // Оператор для режима оператора
	// Для управления кешированием первого взаимодействия
	redisCache CacheMethods
	// Получение ключа пользователя
	rpc                  ORCClient
	clients              sync.Map // key: userId (uint32), value: *Client
	operatorModeByDialog sync.Map // key: dialogId (uint64), value: bool
	pendingOAuthSettings sync.Map // key: userId (uint32), value: *domainavito.PendingOAuthSettings - временное хранилище настроек до завершения OAuth
	// Для однократной отправки уведомления о необходимости авториоваться в Landing для получения userKey
	needReauthorization map[uint32]time.Time
}
