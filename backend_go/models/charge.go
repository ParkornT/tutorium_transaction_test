package models

// PaymentRequest is the payload from your frontend to initiate a charge.
type PaymentRequest struct {
	Amount      int64                  `json:"amount"`               // (satang unit : 100 satang = 1 THB)
	Currency    string                 `json:"currency"`             // "THB"
	PaymentType string                 `json:"paymentType"`          // "credit_card" | "promptpay" | "internet_banking"
	Token       string                 `json:"token,omitempty"`      // for card charges (preferred)
	ReturnURI   string                 `json:"return_uri,omitempty"` // required for some redirects (3DS/internet banking)
	Description string                 `json:"description,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"` // free-form, attached to the Omise charge
	Card        map[string]interface{} `json:"card,omitempty"`     // server-side tokenization (TESTING ONLY)
	Bank        string                 `json:"bank,omitempty"`     // e.g. "bbl", "bay", "scb"
	UserID      *uint                  `json:"user_id,omitempty"`  // FK to users.id
}
