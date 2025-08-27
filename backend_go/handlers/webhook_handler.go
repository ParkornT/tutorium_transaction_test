package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	omise "github.com/omise/omise-go"
	"github.com/omise/omise-go/operations"
)

type WebhookHandler struct {
	client    *omise.Client
	dbHandler interface {
		UpsertTransactionFromCharge(*omise.Charge) error
	}
}

func NewWebhookHandler(client *omise.Client, dbHandler interface {
	UpsertTransactionFromCharge(*omise.Charge) error
}) *WebhookHandler {
	return &WebhookHandler{client: client, dbHandler: dbHandler}
}

// HandleWebhook verifies event by ID via Omise Events API, then reacts to charge events.
func (h *WebhookHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Minimal envelope to pull event ID out of the incoming payload
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.ID == "" {
		http.Error(w, "invalid payload: missing event id", http.StatusBadRequest)
		return
	}

	// Verify: retrieve the event back from Omise using your secret key
	// Verify: retrieve the event back from Omise
	ev := &omise.Event{}
	if err := h.client.Do(ev, &operations.RetrieveEvent{EventID: envelope.ID}); err != nil {
		log.Printf("Webhook: event verification failed for id=%s: %v", envelope.ID, err)
		http.Error(w, "event verification failed", http.StatusBadRequest)
		return
	}

	// For charge-related events
	switch ev.Key {
	case "charge.complete", "charge.capture", "charge.failed", "charge.expired", "charge.reversed":
		raw, err := json.Marshal(ev.Data)
		if err != nil {
			log.Printf("Webhook: marshal ev.Data failed: %v", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		var data struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		}
		if err := json.Unmarshal(raw, &data); err != nil || data.Object != "charge" || data.ID == "" {
			log.Printf("Webhook: unexpected event data for key=%s; data=%s", ev.Key, string(raw))
			w.WriteHeader(http.StatusOK)
			return
		}

		ch := &omise.Charge{}
		if err := h.client.Do(ch, &operations.RetrieveCharge{ChargeID: data.ID}); err != nil {
			log.Printf("Webhook: retrieve charge %s failed: %v", data.ID, err)
			w.WriteHeader(http.StatusOK)
			return
		}

		if err := h.dbHandler.UpsertTransactionFromCharge(ch); err != nil {
			log.Printf("Failed to upsert transaction: %v", err)
		}
		log.Printf("Webhook: charge %s status=%s amount=%d source=%v", ch.ID, ch.Status, ch.Amount, ch.Source)
	}
}
