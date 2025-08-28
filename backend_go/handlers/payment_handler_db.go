// payment_handler_db.go contains GET and POST handlers for /payments and /transactions
package handlers

import (
	"errors"
	"log"
	"strconv"

	"github.com/a2n2k3p4/tutorium-backend/models"
	"github.com/gofiber/fiber/v2"
	omise "github.com/omise/omise-go"
	"github.com/omise/omise-go/operations"
	"gorm.io/gorm"
)

func (h *PaymentHandler) CreateCharge(c *fiber.Ctx) error {
	var req models.PaymentRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid request: " + err.Error()})
	}
	if req.Amount <= 0 || req.Currency == "" {
		return c.Status(400).JSON(fiber.Map{"error": "amount and currency are required"})
	}

	// Try to resolve user id from body/header/query
	userID := h.getUserIDFromRequest(c, &req)

	var (
		charge *omise.Charge
		err    error
	)
	switch req.PaymentType {
	case "credit_card":
		charge, err = h.processCreditCard(req)
	case "promptpay":
		charge, err = h.processPromptPay(req)
	case "internet_banking":
		charge, err = h.processInternetBanking(req)
	default:
		return c.Status(400).JSON(fiber.Map{"error": "unsupported paymentType: " + req.PaymentType})
	}
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}

	// Persist/Upsert a local transaction row (idempotent on charge_id)
	if err := h.upsertTransactionFromCharge(charge, userID); err != nil {
		log.Printf("Failed to save transaction: %v", err) // do not fail outward
	}

	return c.JSON(charge)
}

func (h *PaymentHandler) createCharge(op *operations.CreateCharge) (*omise.Charge, error) {
	ch := &omise.Charge{}
	if err := h.Client.Do(ch, op); err != nil {
		return nil, err
	}
	return ch, nil
}

func (h *PaymentHandler) ListTransactions(c *fiber.Ctx) error {
	f := txFilters{
		UserID:  c.Query("user_id"),
		Status:  c.Query("status"),
		Channel: c.Query("channel"),
	}
	limit, offset := helpersParseLimitOffset(c.Query("limit"), c.Query("offset"))

	// count
	var totalCount int64
	if err := h.DB.Model(&models.Transaction{}).
		Scopes(helpersApplyTxFilters(f)).
		Count(&totalCount).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to count transactions: " + err.Error()})
	}

	// data (fresh query) — GORM scope keeps this concise. :contentReference[oaicite:3]{index=3}
	var transactions []models.Transaction
	if err := h.DB.Model(&models.Transaction{}).
		Scopes(helpersApplyTxFilters(f)).
		Preload("User").
		Order("created_at DESC").
		Limit(limit).Offset(offset).
		Find(&transactions).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to retrieve transactions: " + err.Error()})
	}

	return c.JSON(fiber.Map{
		"transactions": transactions,
		"pagination": fiber.Map{
			"total":  totalCount,
			"limit":  limit,
			"offset": offset,
		},
	})
}

func (h *PaymentHandler) GetTransaction(c *fiber.Ctx) error {
	id := c.Params("id")
	if id == "" {
		return c.Status(400).JSON(fiber.Map{"error": "id is required"})
	}

	var tx models.Transaction
	// If numeric, treat as internal PK; else treat as ChargeID
	if n, err := strconv.ParseUint(id, 10, 64); err == nil {
		err = h.DB.Preload("User").First(&tx, uint(n)).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to retrieve transaction: " + err.Error()})
		}
		if err == nil {
			return c.JSON(tx)
		}
	}

	// Fallback to ChargeID lookup
	if err := h.DB.Preload("User").Where("charge_id = ?", id).First(&tx).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.Status(404).JSON(fiber.Map{"error": "Transaction not found"})
		}
		return c.Status(500).JSON(fiber.Map{"error": "Failed to retrieve transaction: " + err.Error()})
	}
	return c.JSON(tx)
}
