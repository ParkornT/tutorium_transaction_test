package models

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Transaction struct {
	ID             uint              `gorm:"primaryKey" json:"id"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	DeletedAt      gorm.DeletedAt    `gorm:"index" json:"-"`
	UserID         *uint             `gorm:"index" json:"user_id,omitempty"`
	ChargeID       string            `gorm:"uniqueIndex" json:"charge_id"`
	AmountSatang   int64             `json:"amount_satang"`
	Currency       string            `json:"currency"`
	Channel        string            `json:"channel"`
	Status         string            `json:"status"`
	FailureCode    *string           `json:"failure_code,omitempty"`
	FailureMessage *string           `json:"failure_message,omitempty"`
	RawPayload     []byte            `json:"-"`
	Meta           datatypes.JSONMap `gorm:"type:jsonb" json:"meta,omitempty"`

	User *User `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"-"`
}
