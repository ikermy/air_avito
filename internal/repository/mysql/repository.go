package mysql

import (
	"air_avito/internal/domain"
	"air_avito/internal/repository"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ikermy/air_common/pkg/comdb"
	"github.com/ikermy/air_common/pkg/crypto"
	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_common/pkg/model/commdom"
	"github.com/ikermy/air_common/pkg/model/create"
	"github.com/ikermy/air_logger/v2/pkg/logger"
)

type Implementation struct {
	db *comdb.DB
}

func New(db *comdb.DB) (repository.Repository, error) {
	if db == nil {
		return repository.Repository{}, fmt.Errorf("database connection is nil")
	}

	repo := &Implementation{db: db}

	return repository.Repository{
		Internal: repo,
		External: db,
	}, nil
}

type NullBytes struct {
	Bytes []byte
	Valid bool
}

func (nb *NullBytes) Scan(value any) error {
	if value == nil {
		nb.Bytes = nil
		nb.Valid = false
		return nil
	}

	switch v := value.(type) {
	case []byte:
		nb.Bytes = v
		nb.Valid = true
	default:
		return fmt.Errorf("невозможно преобразовать тип %T в NullBytes", value)
	}
	return nil
}

func populateUserDetails(user *domain.UserDetails, name, assistantID sql.NullString, provider sql.NullByte, data NullBytes, start, end, target sql.NullBool) {
	if assistantID.Valid {
		user.AssistantID = assistantID.String
	}

	if name.Valid {
		user.AssistName = name.String
	}

	if provider.Valid {
		user.Provider = commdom.ProviderType(provider.Byte)
	}

	if data.Valid {
		mdata, err := create.DecompressModelData(data.Bytes)
		if err == nil {
			user.MetaAction = mdata.MetaAction
			user.Triggers = mdata.Triggers
			user.AskLimit = uint32(mdata.Espero.Limit)
			user.Espero = mdata.Espero.Wait
			user.Ignore = mdata.Espero.Ignore
		}
	}

	if start.Valid {
		user.Events.Start = start.Bool
	}
	if end.Valid {
		user.Events.End = end.Bool
	}
	if target.Valid {
		user.Events.Target = target.Bool
	}
}

func decryptSessionData(mk [32]byte, raw string) (string, error) {
	decrypted, err := crypto.DecryptFieldWithMasterKey(mk, raw)
	if err != nil {
		return "", fmt.Errorf("ошибка расшифрования токена: %w", err)
	}

	return decrypted, nil
}

func (r *Implementation) GetAvitoToken(ctx context.Context, userID uint32, mk [32]byte) (*domain.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, mode.GetSQLTimeToCancel())
	defer cancel()

	query := `SELECT Avito FROM channels WHERE UserId = ? AND Avito_enabled = 1`

	var avitoJSON sql.NullString
	err := r.db.Conn().QueryRowContext(ctx, query, userID).Scan(&avitoJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("avito токен для пользователя %d не найден", userID)
		}
		logger.Error("Ошибка получения Avito токена из БД: %v", err, userID)
		return nil, err
	}

	// Если поле зашифровано MasterKey — расшифровываем
	if crypto.IsEncryptedWithMasterKey(avitoJSON.String) {
		encrypted, err := decryptSessionData(mk, avitoJSON.String)
		if err != nil {
			return nil, fmt.Errorf("ошибка расшифровки данных сессии: %v", err)
		}
		// Обновляю данные сессии на расшифрованные
		avitoJSON.String = encrypted
	}

	if !avitoJSON.Valid || avitoJSON.String == "" {
		return nil, fmt.Errorf("avito токен для пользователя %d пустой", userID)
	}

	var token domain.Token
	if err := json.Unmarshal([]byte(avitoJSON.String), &token); err != nil {
		logger.Error("Ошибка парсинга Avito JSON: %v", err, userID)
		return nil, fmt.Errorf("ошибка парсинга токена: %w", err)
	}

	return &token, nil
}

func (r *Implementation) SaveAvitoToken(ctx context.Context, userID uint32, mk [32]byte, token domain.Token) error {
	ctx, cancel := context.WithTimeout(ctx, mode.GetSQLTimeToCancel())
	defer cancel()

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		logger.Error("Ошибка сериализации Avito токена: %v", err, userID)
		return err
	}

	// Шифруем данные канала MasterKey'ом ($mk$) если он доступен
	var zeroKey [32]byte
	if !bytes.Equal(mk[:], zeroKey[:]) {
		encrypted, err := crypto.EncryptFieldWithMasterKey(mk, string(tokenJSON))
		if err != nil {
			return fmt.Errorf("failed to encrypt channel data with MasterKey: %w", err)
		}
		tokenJSON = []byte(encrypted)
	}

	query := `UPDATE channels SET Avito = ?, Avito_enabled = 1 WHERE UserId = ?`
	result, err := r.db.Conn().ExecContext(ctx, query, string(tokenJSON), userID)
	if err != nil {
		logger.Error("Ошибка сохранения Avito токена в БД: %v", err, userID)
		return err
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("пользователь %d не найден в channels", userID)
	}

	logger.Info("Avito токен успешно сохранен", userID)
	return nil
}

func (r *Implementation) UpdateAvitoToken(ctx context.Context, userID uint32, mk [32]byte, accessToken string, expiry time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, mode.GetSQLTimeToCancel())
	defer cancel()

	currentToken, err := r.GetAvitoToken(ctx, userID, mk)
	if err != nil {
		return err
	}

	currentToken.AccessToken = accessToken
	currentToken.Expiry = expiry

	tokenJSON, err := json.Marshal(currentToken)
	if err != nil {
		logger.Error("Ошибка сериализации обновленного Avito токена: %v", err, userID)
		return err
	}

	// Шифруем данные канала MasterKey'ом ($mk$) если он доступен
	var zeroKey [32]byte
	if !bytes.Equal(mk[:], zeroKey[:]) {
		encrypted, err := crypto.EncryptFieldWithMasterKey(mk, string(tokenJSON))
		if err != nil {
			return fmt.Errorf("failed to encrypt channel data with MasterKey: %w", err)
		}
		tokenJSON = []byte(encrypted)
	}

	query := `UPDATE channels SET Avito = ? WHERE UserId = ?`
	result, err := r.db.Conn().ExecContext(ctx, query, tokenJSON, userID)
	if err != nil {
		logger.Error("Ошибка обновления Avito токена в БД: %v", err, userID)
		return err
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("токен для пользователя %d не найден", userID)
	}

	logger.Debug("Avito токен успешно обновлен", userID)
	return nil
}

func (r *Implementation) GetAvitoUsers(ctx context.Context) ([]domain.UserDetails, error) {
	ctx, cancel := context.WithTimeout(ctx, mode.GetSQLTimeToCancel())
	defer cancel()

	query := `
   SELECT
    c.UserId,
#     c.Avito,
    c.Avito_enabled,
    u_gpt.Name,
    u_gpt.AssistantId,
    u_gpt.Data,
    um.Provider,
    n.Start,
    n.End,
    n.Target
   FROM
    channels AS c
   LEFT JOIN
    user_models AS um ON c.UserId = um.UserId AND um.IsActive = 1
   LEFT JOIN
    user_gpt AS u_gpt ON um.ModelId = u_gpt.Id
   LEFT JOIN
    notifications AS n ON c.UserId = n.UserId
   WHERE
    c.Avito IS NOT NULL AND c.Avito_enabled = 1`

	rows, err := r.db.Conn().QueryContext(ctx, query)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при получении пользователей Avito: %w", mode.GetSQLTimeToCancel(), err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена при получении пользователей Avito: %w", err)
		default:
			return nil, fmt.Errorf("failed to execute query for Avito users: %w", err)
		}
	}
	defer func(rows *sql.Rows) {
		err := rows.Close()
		if err != nil {
			logger.Info("ошибка закрытия rows: %v", err)
		}
	}(rows)

	var users []domain.UserDetails
	for rows.Next() {
		var user domain.UserDetails
		var name, assistantID sql.NullString
		var provider sql.NullByte
		var data NullBytes
		var start, end, target sql.NullBool

		err := rows.Scan(
			&user.UserID,
			//&user.Avito,
			&user.AvitoEnabled,
			&name,
			&assistantID,
			&data,
			&provider,
			&start,
			&end,
			&target,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		populateUserDetails(&user, name, assistantID, provider, data, start, end, target)
		users = append(users, user)
	}

	if err = rows.Err(); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при обработке результатов пользователей Avito: %w", mode.GetSQLTimeToCancel(), err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена при обработке результатов пользователей Avito: %w", err)
		default:
			return nil, fmt.Errorf("error iterating rows: %w", err)
		}
	}

	return users, nil
}

func (r *Implementation) GetAvitoUser(ctx context.Context, userID uint32) (*domain.UserDetails, error) {
	ctx, cancel := context.WithTimeout(ctx, mode.GetSQLTimeToCancel())
	defer cancel()

	query := `
   SELECT
    c.UserId,
#     c.Avito,
    c.Avito_enabled,
    u_gpt.Name,
    u_gpt.AssistantId,
    u_gpt.Data,
    um.Provider,
    n.Start,
    n.End,
    n.Target
   FROM
    channels AS c
   LEFT JOIN
    user_models AS um ON c.UserId = um.UserId AND um.IsActive = 1
   LEFT JOIN
    user_gpt AS u_gpt ON um.ModelId = u_gpt.Id
   LEFT JOIN
    notifications AS n ON c.UserId = n.UserId
   WHERE
    c.UserId = ? AND c.Avito IS NOT NULL AND c.Avito_enabled = 1`

	var user domain.UserDetails
	var name, assistantID sql.NullString
	var provider sql.NullByte
	var data NullBytes
	var start, end, target sql.NullBool

	err := r.db.Conn().QueryRowContext(ctx, query, userID).Scan(
		&user.UserID,
		//&user.Avito,
		&user.AvitoEnabled,
		&name,
		&assistantID,
		&data,
		&provider,
		&start,
		&end,
		&target,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("пользователь %d не найден или Avito не активирован", userID)
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при получении пользователя Avito: %w", mode.GetSQLTimeToCancel(), err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена при получении пользователя Avito: %w", err)
		default:
			return nil, fmt.Errorf("failed to get Avito user: %w", err)
		}
	}

	populateUserDetails(&user, name, assistantID, provider, data, start, end, target)
	return &user, nil
}

// GetAvitoToken получает OAuth токен для пользователя из channels.Avito
//func (d *DB) GetAvitoToken(userId uint32) (*AvitoToken, error) {
//	ctx, cancel := context.WithTimeout(d.Context(), time.Duration(sqlTimeToCancel)*time.Second)
//	defer cancel()
//
//	query := `SELECT Avito FROM channels WHERE UserId = ? AND Avito_enabled = 1`
//
//	var avitoJSON sql.NullString
//	err := d.Conn().QueryRowContext(ctx, query, userId).Scan(&avitoJSON)
//
//	if err != nil {
//		if errors.Is(err, sql.ErrNoRows) {
//			return nil, fmt.Errorf("Avito токен для пользователя %d не найден", userId)
//		}
//		logger.Error("Ошибка получения Avito токена из БД: %v", err, userId)
//		return nil, err
//	}
//
//	if !avitoJSON.Valid || avitoJSON.String == "" {
//		return nil, fmt.Errorf("Avito токен для пользователя %d пустой", userId)
//	}
//
//	var token AvitoToken
//	if err := json.Unmarshal([]byte(avitoJSON.String), &token); err != nil {
//		logger.Error("Ошибка парсинга Avito JSON: %v", err, userId)
//		return nil, fmt.Errorf("ошибка парсинга токена: %w", err)
//	}
//
//	return &token, nil
//}
//
//// SaveAvitoToken сохраняет новый OAuth токен в channels.Avito
//func (d *DB) SaveAvitoToken(userId uint32, avitoUserID, accessToken, refreshToken, tokenType string, expiry time.Time, scopes json.RawMessage, clientID, clientSecret, redirectUrlPrefix string) error {
//	ctx, cancel := context.WithTimeout(d.Context(), time.Duration(sqlTimeToCancel)*time.Second)
//	defer cancel()
//
//	// Парсим scopes из JSON
//	var scopesList []string
//	if len(scopes) > 0 {
//		if err := json.Unmarshal(scopes, &scopesList); err != nil {
//			logger.Error("Ошибка парсинга scopes: %v", err, userId)
//			return err
//		}
//	}
//
//	token := AvitoToken{
//		AvitoUserID:       avitoUserID,
//		AccessToken:       accessToken,
//		RefreshToken:      refreshToken,
//		TokenType:         tokenType,
//		Expiry:            expiry,
//		Scopes:            scopesList,
//		ClientID:          clientID,
//		ClientSecret:      clientSecret,
//		RedirectUrlPrefix: redirectUrlPrefix,
//	}
//
//	tokenJSON, err := json.Marshal(token)
//	if err != nil {
//		logger.Error("Ошибка сериализации Avito токена: %v", err, userId)
//		return err
//	}
//
//	query := `UPDATE channels SET Avito = ?, Avito_enabled = 1 WHERE UserId = ?`
//
//	result, err := d.Conn().ExecContext(ctx, query, string(tokenJSON), userId)
//	if err != nil {
//		logger.Error("Ошибка сохранения Avito токена в БД: %v", err, userId)
//		return err
//	}
//
//	rowsAffected, _ := result.RowsAffected()
//	if rowsAffected == 0 {
//		return fmt.Errorf("пользователь %d не найден в channels", userId)
//	}
//
//	logger.Info("Avito токен успешно сохранен", userId)
//	return nil
//}
//
//// UpdateAvitoToken обновляет access token и expiry в channels.Avito
//func (d *DB) UpdateAvitoToken(userId uint32, accessToken string, expiry time.Time) error {
//	ctx, cancel := context.WithTimeout(d.Context(), time.Duration(sqlTimeToCancel)*time.Second)
//	defer cancel()
//
//	// Сначала получаем текущий токен
//	currentToken, err := d.GetAvitoToken(userId)
//	if err != nil {
//		return err
//	}
//
//	// Обновляем только access_token и expiry
//	currentToken.AccessToken = accessToken
//	currentToken.Expiry = expiry
//
//	tokenJSON, err := json.Marshal(currentToken)
//	if err != nil {
//		logger.Error("Ошибка сериализации обновленного Avito токена: %v", err, userId)
//		return err
//	}
//
//	query := `UPDATE channels SET Avito = ? WHERE UserId = ?`
//
//	result, err := d.Conn().ExecContext(ctx, query, string(tokenJSON), userId)
//	if err != nil {
//		logger.Error("Ошибка обновления Avito токена в БД: %v", err, userId)
//		return err
//	}
//
//	rowsAffected, _ := result.RowsAffected()
//	if rowsAffected == 0 {
//		return fmt.Errorf("токен для пользователя %d не найден", userId)
//	}
//
//	logger.Debug("Avito токен успешно обновлен", userId)
//	return nil
//}
//
//// GetAvitoUsers получает список пользователей с активной Avito интеграцией
//func (d *DB) GetAvitoUsers() ([]UserDetails, error) {
//	// Дочерний контекст с тайм-аутом на операцию
//	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
//	defer cancel()
//
//	// Получаем пользователей с включенным Avito каналом
//	query := `
//   SELECT
//    c.UserId,
//    c.Avito,
//    c.Avito_enabled,
//    u_gpt.Name,
//    u_gpt.AssistantId,
//    u_gpt.Data,
//    um.Provider,
//    n.Start,
//    n.End,
//    n.Target
//   FROM
//    channels AS c
//   LEFT JOIN
//    user_models AS um ON c.UserId = um.UserId AND um.IsActive = 1
//   LEFT JOIN
//    user_gpt AS u_gpt ON um.ModelId = u_gpt.Id
//   LEFT JOIN
//    notifications AS n ON c.UserId = n.UserId
//   WHERE
//    c.Avito IS NOT NULL AND c.Avito_enabled = 1`
//
//	rows, err := d.Conn().QueryContext(ctx, query)
//	if err != nil {
//		switch {
//		case errors.Is(err, context.DeadlineExceeded):
//			return nil, fmt.Errorf("тайм-аут (%d с) при получении пользователей Avito: %w", sqlTimeToCancel, err)
//		case errors.Is(err, context.Canceled):
//			return nil, fmt.Errorf("операция отменена при получении пользователей Avito: %w", err)
//		default:
//			return nil, fmt.Errorf("failed to execute query for Avito users: %w", err)
//		}
//	}
//	defer func(rows *sql.Rows) {
//		err := rows.Close()
//		if err != nil {
//			logger.Info("ошибка закрытия rows: %v", err)
//		}
//	}(rows)
//
//	// Собираем результаты
//	var users []UserDetails
//	for rows.Next() {
//		var user UserDetails
//		var Name, assistantId sql.NullString
//		var provider sql.NullByte
//		var data NullBytes
//		var start, end, target sql.NullBool
//
//		err := rows.Scan(
//			&user.UserId,
//			&user.Avito,
//			&user.AvitoEnabled,
//			&Name,
//			&assistantId,
//			&data,
//			&provider,
//			&start,
//			&end,
//			&target,
//		)
//		if err != nil {
//			return nil, fmt.Errorf("failed to scan row: %w", err)
//		}
//
//		// Заполняем данные пользователя через общую функцию
//		populateUserDetails(&user, Name, assistantId, provider, data, start, end, target)
//
//		users = append(users, user)
//	}
//
//	// Проверяем ошибки после обработки результатов
//	if err = rows.Err(); err != nil {
//		switch {
//		case errors.Is(err, context.DeadlineExceeded):
//			return nil, fmt.Errorf("тайм-аут (%d с) при обработке результатов пользователей Avito: %w", sqlTimeToCancel, err)
//		case errors.Is(err, context.Canceled):
//			return nil, fmt.Errorf("операция отменена при обработке результатов пользователей Avito: %w", err)
//		default:
//			return nil, fmt.Errorf("error iterating rows: %w", err)
//		}
//	}
//
//	return users, nil
//}
//
//// GetAvitoUser получает данные конкретного пользователя с активной Avito интеграцией
//func (d *DB) GetAvitoUser(userId uint32) (*UserDetails, error) {
//	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
//	defer cancel()
//
//	query := `
//   SELECT
//    c.UserId,
//    c.Avito,
//    c.Avito_enabled,
//    u_gpt.Name,
//    u_gpt.AssistantId,
//    u_gpt.Data,
//    um.Provider,
//    n.Start,
//    n.End,
//    n.Target
//   FROM
//    channels AS c
//   LEFT JOIN
//    user_models AS um ON c.UserId = um.UserId AND um.IsActive = 1
//   LEFT JOIN
//    user_gpt AS u_gpt ON um.ModelId = u_gpt.Id
//   LEFT JOIN
//    notifications AS n ON c.UserId = n.UserId
//   WHERE
//    c.UserId = ? AND c.Avito IS NOT NULL AND c.Avito_enabled = 1`
//
//	var user UserDetails
//	var Name, assistantId sql.NullString
//	var provider sql.NullByte
//	var data NullBytes
//	var start, end, target sql.NullBool
//
//	err := d.Conn().QueryRowContext(ctx, query, userId).Scan(
//		&user.UserId,
//		&user.Avito,
//		&user.AvitoEnabled,
//		&Name,
//		&assistantId,
//		&data,
//		&provider,
//		&start,
//		&end,
//		&target,
//	)
//
//	if err != nil {
//		if errors.Is(err, sql.ErrNoRows) {
//			return nil, fmt.Errorf("пользователь %d не найден или Avito не активирован", userId)
//		}
//		switch {
//		case errors.Is(err, context.DeadlineExceeded):
//			return nil, fmt.Errorf("тайм-аут (%d с) при получении пользователя Avito: %w", sqlTimeToCancel, err)
//		case errors.Is(err, context.Canceled):
//			return nil, fmt.Errorf("операция отменена при получении пользователя Avito: %w", err)
//		default:
//			return nil, fmt.Errorf("failed to get Avito user: %w", err)
//		}
//	}
//
//	populateUserDetails(&user, Name, assistantId, provider, data, start, end, target)
//	return &user, nil
//}

// populateUserDetails заполняет структуру UserDetails из NULL-безопасных значений базы данных
//func populateUserDetails(user *UserDetails, Name, assistantId sql.NullString, provider sql.NullByte, data NullBytes, start, end, target sql.NullBool) {
//	// Обработка NULL значений
//	if assistantId.Valid {
//		user.AssistantId = assistantId.String
//	}
//
//	if Name.Valid {
//		user.AssistName = Name.String
//	}
//
//	// Обработка информации о провайдере
//	if provider.Valid {
//		user.Provider = commdom.ProviderType(provider.Byte)
//	} else {
//		user.Provider = commdom.ProviderOpenAI // 1 = OpenAI по умолчанию
//	}
//
//	// Распаковываем и обрабатываем data если она существует
//	if data.Valid {
//		metaAction, triggers, esperoData, _, _, _, _, _, _, _, _, err := comdb.DecompressAndExtractMetadata(data.Bytes)
//		if err == nil {
//			user.MetaAction = metaAction
//			user.Triggers = triggers
//
//			// Заполняем поля из структуры Espero
//			if esperoData != nil {
//				user.AskLimit = uint32(esperoData.Limit)
//				user.Espero = esperoData.Wait
//				user.Ignore = esperoData.Ignore
//			}
//		}
//	}
//
//	// Обработка полей уведомлений
//	if start.Valid {
//		user.Events.Start = start.Bool
//	}
//	if end.Valid {
//		user.Events.End = end.Bool
//	}
//	if target.Valid {
//		user.Events.Target = target.Bool
//	}
//}
