package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/a2n2k3p4/tutorium-backend/models"
	"github.com/gofiber/fiber/v2"
	omise "github.com/omise/omise-go"
	"github.com/omise/omise-go/operations"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PaymentHandler struct {
	DB     *gorm.DB
	Client *omise.Client
}

func NewPaymentHandler(db *gorm.DB, client *omise.Client) *PaymentHandler {
	return &PaymentHandler{DB: db, Client: client}
}

func (h *PaymentHandler) Health(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok"})
}

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

// Webhook stays here (Fiber). Verifies event, retrieves Charge, upserts local Transaction.
func (h *PaymentHandler) HandleWebhook(c *fiber.Ctx) error {
	var envelope struct {
		ID string `json:"id"`
	}
	if err := c.BodyParser(&envelope); err != nil || envelope.ID == "" {
		return c.Status(400).JSON(fiber.Map{"error": "invalid payload: missing event id"})
	}

	ev := &omise.Event{}
	if err := h.Client.Do(ev, &operations.RetrieveEvent{EventID: envelope.ID}); err != nil {
		log.Printf("Webhook: event verification failed for id=%s: %v", envelope.ID, err)
		return c.Status(400).JSON(fiber.Map{"error": "event verification failed"})
	}

	switch ev.Key {
	// Omise recommends verifying the charge on receipt of charge.complete etc. :contentReference[oaicite:0]{index=0}
	case "charge.complete", "charge.capture", "charge.failed", "charge.expired", "charge.reversed":
		raw, err := json.Marshal(ev.Data)
		if err != nil {
			log.Printf("Webhook: marshal ev.Data failed: %v", err)
			return c.SendStatus(200)
		}
		var data struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		}
		if err := json.Unmarshal(raw, &data); err != nil || data.Object != "charge" || data.ID == "" {
			log.Printf("Webhook: unexpected event data for key=%s; data=%s", ev.Key, string(raw))
			return c.SendStatus(200)
		}

		ch := &omise.Charge{}
		if err := h.Client.Do(ch, &operations.RetrieveCharge{ChargeID: data.ID}); err != nil {
			log.Printf("Webhook: retrieve charge %s failed: %v", data.ID, err)
			return c.SendStatus(200)
		}
		if err := h.upsertTransactionFromCharge(ch, nil); err != nil {
			log.Printf("Webhook: failed to upsert transaction: %v", err)
		}
		log.Printf("Webhook: processed charge %s status=%s", ch.ID, ch.Status)
	}
	return c.SendStatus(200)
}

func (h *PaymentHandler) ListTransactions(c *fiber.Ctx) error {
	// Filters
	userID := c.Query("user_id")
	status := c.Query("status")
	channel := c.Query("channel")
	limitStr := c.Query("limit")
	offsetStr := c.Query("offset")

	limit, offset := 50, 0
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}
	if offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	base := h.DB.Model(&models.Transaction{})
	if userID != "" {
		base = base.Where("user_id = ?", userID)
	}
	if status != "" {
		base = base.Where("status = ?", status)
	}
	if channel != "" {
		base = base.Where("channel = ?", channel)
	}

	var totalCount int64
	if err := base.Count(&totalCount).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to count transactions: " + err.Error()})
	}

	// Create a fresh query for data to avoid side-effects from Count
	query := h.DB.Model(&models.Transaction{})
	if userID != "" {
		query = query.Where("user_id = ?", userID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if channel != "" {
		query = query.Where("channel = ?", channel)
	}

	var transactions []models.Transaction
	if err := query.Preload("User").
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

// ----------------- Payment processors -----------------

func (h *PaymentHandler) processCreditCard(req models.PaymentRequest) (*omise.Charge, error) {
	// Attach user_id to metadata if present (Omise supports custom metadata). :contentReference[oaicite:1]{index=1}
	metadata := req.Metadata
	if req.UserID != nil {
		if metadata == nil {
			metadata = make(map[string]interface{})
		}
		metadata["user_id"] = fmt.Sprintf("%d", *req.UserID)
	}

	// Preferred flow: card token already created by frontend (Omise.js / mobile SDK). :contentReference[oaicite:2]{index=2}
	if req.Token != "" {
		return h.createCharge(&operations.CreateCharge{
			Amount:      req.Amount,
			Currency:    req.Currency,
			Card:        req.Token,
			ReturnURI:   req.ReturnURI,
			Description: req.Description,
			Metadata:    metadata,
		})
	}

	// Server-side tokenization (testing only)
	if req.Card == nil {
		return nil, fmt.Errorf("missing token; either provide token or card for tokenization")
	}
	name, _ := req.Card["name"].(string)
	number, _ := req.Card["number"].(string)

	var expMonth, expYear int
	var securityCode string

	switch v := req.Card["expiration_month"].(type) {
	case float64:
		expMonth = int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid expiration_month: %v", v)
		}
		expMonth = n
	default:
		return nil, fmt.Errorf("unexpected type for expiration_month: %T", v)
	}
	switch v := req.Card["expiration_year"].(type) {
	case float64:
		expYear = int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid expiration_year: %v", v)
		}
		expYear = n
	default:
		return nil, fmt.Errorf("unexpected type for expiration_year: %T", v)
	}
	switch v := req.Card["security_code"].(type) {
	case string:
		securityCode = v
	case float64:
		securityCode = strconv.Itoa(int(v))
	default:
		return nil, fmt.Errorf("unexpected type for security_code: %T", v)
	}

	token := &omise.Token{}
	if err := h.Client.Do(token, &operations.CreateToken{
		Name:            name,
		Number:          number,
		ExpirationMonth: time.Month(expMonth),
		ExpirationYear:  expYear,
		SecurityCode:    securityCode,
	}); err != nil {
		return nil, fmt.Errorf("failed to create token: %v", err)
	}

	return h.createCharge(&operations.CreateCharge{
		Amount:      req.Amount,
		Currency:    req.Currency,
		Card:        token.ID,
		ReturnURI:   req.ReturnURI,
		Description: req.Description,
		Metadata:    metadata,
	})
}

func (h *PaymentHandler) processPromptPay(req models.PaymentRequest) (*omise.Charge, error) {
	// Create a source with type "promptpay", then create a charge from it. :contentReference[oaicite:3]{index=3}
	metadata := req.Metadata
	if req.UserID != nil {
		if metadata == nil {
			metadata = make(map[string]interface{})
		}
		metadata["user_id"] = fmt.Sprintf("%d", *req.UserID)
	}

	src := &omise.Source{}
	if err := h.Client.Do(src, &operations.CreateSource{
		Type:     "promptpay",
		Amount:   req.Amount,
		Currency: req.Currency,
	}); err != nil {
		return nil, fmt.Errorf("failed to create promptpay source: %v", err)
	}

	return h.createCharge(&operations.CreateCharge{
		Amount:      req.Amount,
		Currency:    req.Currency,
		Source:      src.ID,
		Description: req.Description,
		Metadata:    metadata,
	})
}

func (h *PaymentHandler) processInternetBanking(req models.PaymentRequest) (*omise.Charge, error) {
	// Internet banking requires a source like "internet_banking_bbl", "internet_banking_scb", etc. :contentReference[oaicite:4]{index=4}
	if req.Bank == "" {
		return nil, fmt.Errorf(`bank is required for internet_banking (e.g. "bay", "bbl", "scb")`)
	}
	if req.ReturnURI == "" {
		return nil, fmt.Errorf("return_uri is required for internet_banking")
	}

	metadata := req.Metadata
	if req.UserID != nil {
		if metadata == nil {
			metadata = make(map[string]interface{})
		}
		metadata["user_id"] = fmt.Sprintf("%d", *req.UserID)
	}

	src := &omise.Source{}
	if err := h.Client.Do(src, &operations.CreateSource{
		Type:     "internet_banking_" + req.Bank,
		Amount:   req.Amount,
		Currency: req.Currency,
	}); err != nil {
		return nil, fmt.Errorf("failed to create internet banking source: %v", err)
	}

	return h.createCharge(&operations.CreateCharge{
		Amount:      req.Amount,
		Currency:    req.Currency,
		Source:      src.ID,
		ReturnURI:   req.ReturnURI,
		Description: req.Description,
		Metadata:    metadata,
	})
}

// ----------------- Helpers -----------------

func (h *PaymentHandler) createCharge(op *operations.CreateCharge) (*omise.Charge, error) {
	ch := &omise.Charge{}
	if err := h.Client.Do(ch, op); err != nil {
		return nil, err
	}
	return ch, nil
}

func (h *PaymentHandler) upsertTransactionFromCharge(charge *omise.Charge, userID *uint) error {
	if charge == nil {
		return fmt.Errorf("nil charge")
	}
	userID = extractUserIDFromCharge(charge, userID)
	channel := determineChannel(charge)
	rawPayload, _ := json.Marshal(charge)

	var meta datatypes.JSONMap
	if charge.Metadata != nil {
		meta = datatypes.JSONMap(charge.Metadata)
	}

	transaction := models.Transaction{
		UserID:         userID,
		ChargeID:       charge.ID,
		AmountSatang:   charge.Amount,
		Currency:       charge.Currency,
		Channel:        channel,
		Status:         string(charge.Status),
		FailureCode:    charge.FailureCode,
		FailureMessage: charge.FailureMessage,
		RawPayload:     rawPayload,
		Meta:           meta,
	}

	if err := h.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "charge_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"status", "failure_code", "failure_message",
			"amount_satang", "currency", "channel",
			"raw_payload", "meta", "updated_at", "user_id",
		}),
	}).Create(&transaction).Error; err != nil {
		return err
	}

	// Update user balance if successful
	if charge.Status == "successful" && userID != nil {
		amountTHB := float64(charge.Amount) / 100.0
		if err := h.DB.Model(&models.User{}).
			Where("id = ?", *userID).
			Update("balance", gorm.Expr("balance + ?", amountTHB)).Error; err != nil {
			log.Printf("Failed to update user balance: %v", err)
		}
	}
	return nil
}

func extractUserIDFromCharge(charge *omise.Charge, userID *uint) *uint {
	if userID != nil || charge == nil || charge.Metadata == nil {
		return userID
	}
	if v, ok := charge.Metadata["user_id"]; ok {
		switch vv := v.(type) {
		case string:
			if n, err := strconv.ParseUint(vv, 10, 32); err == nil {
				u := uint(n)
				return &u
			}
		case float64:
			u := uint(vv)
			return &u
		case int:
			u := uint(vv)
			return &u
		}
	}
	return userID
}

func determineChannel(charge *omise.Charge) string {
	if charge == nil {
		return "card"
	}
	if charge.Source != nil && charge.Source.Type != "" {
		return charge.Source.Type
	}
	return "card"
}

func (h *PaymentHandler) getUserIDFromRequest(c *fiber.Ctx, req *models.PaymentRequest) *uint {
	if req.UserID != nil {
		return req.UserID
	}
	if userIDHeader := c.Get("X-User-ID"); userIDHeader != "" {
		if userID, err := strconv.ParseUint(userIDHeader, 10, 32); err == nil {
			u := uint(userID)
			return &u
		}
	}
	if userIDQuery := c.Query("user_id"); userIDQuery != "" {
		if userID, err := strconv.ParseUint(userIDQuery, 10, 32); err == nil {
			u := uint(userID)
			return &u
		}
	}
	return nil
}
