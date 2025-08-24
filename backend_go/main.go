package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	omise "github.com/omise/omise-go"
	"github.com/omise/omise-go/operations"
)

type PaymentRequest struct {
	Amount      int64                  `json:"amount"`                // subunits (THB: satang)
	Currency    string                 `json:"currency"`              // e.g. "THB"
	PaymentType string                 `json:"paymentType"`           // "credit_card" | "promptpay" | "internet_banking"
	Token       string                 `json:"token,omitempty"`       // preferred for cards
	ReturnURI   string                 `json:"return_uri,omitempty"`  // required for internet_banking, 3DS
	Description string                 `json:"description,omitempty"` // optional
	Metadata    map[string]interface{} `json:"metadata,omitempty"`    // MUST be map[string]interface{} for omise-go

	// test-only fallback if you insist on server-side tokenizing (PCI scope warning)
	Card map[string]interface{} `json:"card,omitempty"`

	// for internet banking, pass bank code suffix such as "bay", "bbl", "scb", ...
	Bank string `json:"bank,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func main() {
	_ = godotenv.Load()

	publicKey := os.Getenv("OMISE_PUBLIC_KEY")
	secretKey := os.Getenv("OMISE_SECRET_KEY")
	if publicKey == "" || secretKey == "" {
		log.Fatal("OMISE_PUBLIC_KEY and OMISE_SECRET_KEY must be set")
	}

	client, err := omise.NewClient(publicKey, secretKey)
	if err != nil {
		log.Fatal("Failed to create Omise client:", err)
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	http.HandleFunc("/charge", func(w http.ResponseWriter, r *http.Request) {
		enableCORS(&w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req PaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "Invalid request: "+err.Error())
			return
		}
		if req.Amount <= 0 || req.Currency == "" {
			jsonErr(w, http.StatusBadRequest, "amount and currency are required")
			return
		}

		var (
			charge *omise.Charge
			perr   error
		)

		switch req.PaymentType {
		case "credit_card":
			charge, perr = processCreditCard(client, req)
		case "promptpay":
			charge, perr = processPromptPay(client, req)
		case "internet_banking":
			charge, perr = processInternetBanking(client, req)
		default:
			jsonErr(w, http.StatusBadRequest, "unsupported paymentType: "+req.PaymentType)
			return
		}

		if perr != nil {
			jsonErr(w, http.StatusInternalServerError, perr.Error())
			return
		}
		jsonResp(w, http.StatusOK, charge)
	})

	fmt.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ----------------- handlers -----------------

func processCreditCard(client *omise.Client, req PaymentRequest) (*omise.Charge, error) {
	// Preferred: charge using a client-created token (Card field is a string token).
	// See omise-go usage sample: CreateCharge has string Card and map[string]interface{} Metadata. :contentReference[oaicite:1]{index=1}
	if req.Token != "" {
		return createCharge(client, &operations.CreateCharge{
			Amount:      req.Amount,
			Currency:    req.Currency,
			Card:        req.Token,     // token like "tokn_test_..."
			ReturnURI:   req.ReturnURI, // needed if your account enforces 3DS
			Description: req.Description,
			Metadata:    req.Metadata, // now correct type
		})
	}

	// Fallback: server-side tokenization (testing or PCI-DSS AoC only). The SDK warns about this. :contentReference[oaicite:2]{index=2}
	if req.Card == nil {
		return nil, fmt.Errorf("missing token; either provide token or card for test-only tokenization")
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
	if err := client.Do(token, &operations.CreateToken{
		Name:            name,
		Number:          number,
		ExpirationMonth: time.Month(expMonth),
		ExpirationYear:  expYear,
		SecurityCode:    securityCode,
	}); err != nil {
		return nil, fmt.Errorf("failed to create token: %v", err)
	}

	return createCharge(client, &operations.CreateCharge{
		Amount:      req.Amount,
		Currency:    req.Currency,
		Card:        token.ID,      // token ID string
		ReturnURI:   req.ReturnURI, // for 3DS if enabled
		Description: req.Description,
		Metadata:    req.Metadata,
	})
}

func processPromptPay(client *omise.Client, req PaymentRequest) (*omise.Charge, error) {
	// Create Source first, then charge with Source ID. Source in CreateCharge is a STRING ID. :contentReference[oaicite:3]{index=3}
	src := &omise.Source{}
	if err := client.Do(src, &operations.CreateSource{
		Type:     "promptpay",
		Amount:   req.Amount,
		Currency: req.Currency,
	}); err != nil {
		return nil, fmt.Errorf("failed to create promptpay source: %v", err)
	}

	return createCharge(client, &operations.CreateCharge{
		Amount:      req.Amount,
		Currency:    req.Currency,
		Source:      src.ID, // string like "src_..."
		Description: req.Description,
		Metadata:    req.Metadata,
	})
}

func processInternetBanking(client *omise.Client, req PaymentRequest) (*omise.Charge, error) {
	if req.Bank == "" {
		return nil, fmt.Errorf("bank is required for internet_banking (e.g. \"bay\", \"bbl\", \"scb\")")
	}
	if req.ReturnURI == "" {
		// Redirect/offsite flow needs a return_uri so the user can be sent back. :contentReference[oaicite:4]{index=4}
		return nil, fmt.Errorf("return_uri is required for internet_banking")
	}

	src := &omise.Source{}
	if err := client.Do(src, &operations.CreateSource{
		Type:     "internet_banking_" + req.Bank,
		Amount:   req.Amount,
		Currency: req.Currency,
	}); err != nil {
		return nil, fmt.Errorf("failed to create internet banking source: %v", err)
	}

	return createCharge(client, &operations.CreateCharge{
		Amount:      req.Amount,
		Currency:    req.Currency,
		Source:      src.ID,        // string, not *omise.Source
		ReturnURI:   req.ReturnURI, // required for redirect methods
		Description: req.Description,
		Metadata:    req.Metadata,
	})
}

// ----------------- helpers -----------------

func createCharge(client *omise.Client, op *operations.CreateCharge) (*omise.Charge, error) {
	ch := &omise.Charge{}
	if err := client.Do(ch, op); err != nil {
		return nil, err
	}
	return ch, nil
}

func enableCORS(w *http.ResponseWriter, r *http.Request) {
	h := (*w).Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

func jsonResp(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonResp(w, code, ErrorResponse{Error: msg})
}
