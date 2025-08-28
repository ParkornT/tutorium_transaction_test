package handlers

import (
	"encoding/json"
	"log"

	"github.com/gofiber/fiber/v2"
	omise "github.com/omise/omise-go"
	"github.com/omise/omise-go/operations"
)

type WebhookHandler struct {
	Client   *omise.Client
	Upserter interface {
		UpsertTransactionFromCharge(*omise.Charge) error
	}
}

func NewWebhookHandler(client *omise.Client, upserter interface {
	UpsertTransactionFromCharge(*omise.Charge) error
}) *WebhookHandler {
	return &WebhookHandler{Client: client, Upserter: upserter}
}

func (h *WebhookHandler) HandleWebhook(c *fiber.Ctx) error {
	// Only POST
	if c.Method() != fiber.MethodPost {
		return c.Status(fiber.StatusMethodNotAllowed).JSON(fiber.Map{"error": "method not allowed"})
	}

	// Minimal envelope
	var envelope struct {
		ID string `json:"id"`
	}
	if err := c.BodyParser(&envelope); err != nil || envelope.ID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid payload: missing event id"})
	}

	// Verify event by fetching from Omise (recommended)
	ev := &omise.Event{}
	if err := h.Client.Do(ev, &operations.RetrieveEvent{EventID: envelope.ID}); err != nil {
		log.Printf("webhook verify failed id=%s err=%v", envelope.ID, err)
		// Bad request will not be retried by Omise; if you want retries, return 5xx here.
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event verification failed"})
	}

	// Only handle events whose data.object == "charge"
	var data struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		log.Printf("webhook marshal ev.Data failed id=%s err=%v", envelope.ID, err)
		return c.SendStatus(fiber.StatusInternalServerError) // trigger retry
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.Object != "charge" || data.ID == "" {
		// ignore non-charge events
		return c.SendStatus(fiber.StatusOK)
	}

	// Retrieve charge (verify status independently)
	ch := &omise.Charge{}
	if err := h.Client.Do(ch, &operations.RetrieveCharge{ChargeID: data.ID}); err != nil {
		log.Printf("webhook retrieve charge failed charge=%s err=%v", data.ID, err)
		return c.SendStatus(fiber.StatusInternalServerError) // trigger retry
	}

	// Idempotent upsert
	if err := h.Upserter.UpsertTransactionFromCharge(ch); err != nil {
		log.Printf("webhook upsert failed charge=%s err=%v", ch.ID, err)
		return c.SendStatus(fiber.StatusInternalServerError) // trigger retry
	}

	log.Printf("webhook processed event=%s key=%s charge=%s status=%s amount=%d source=%v",
		envelope.ID, ev.Key, ch.ID, ch.Status, ch.Amount, ch.Source)

	return c.SendStatus(fiber.StatusOK)
}
