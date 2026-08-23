package main

import (
	"database/sql"
	"time"

	"wish/services/shared/credit"
	"wish/services/shared/marketplace"

	"github.com/google/uuid"
)

// Преобразования между нулевыми значениями модели и NULL в схеме.
// Собраны в одном месте: разбросанные по запросам, они разъезжаются
// с ограничениями схемы при первом же изменении.

func nullAmount(amount credit.Amount) any {
	if amount == 0 {
		return nil
	}
	return int64(amount)
}

func amountOf(value sql.NullInt64) credit.Amount {
	if !value.Valid {
		return 0
	}
	return credit.Amount(value.Int64)
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullUUID(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return *value
}

func marketplaceProvider(value string) marketplace.Provider {
	if value == "" {
		return ""
	}
	return marketplace.Provider(value)
}
