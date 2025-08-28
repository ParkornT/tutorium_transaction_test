package main

import (
	"fmt"
	"log"
	"os"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/joho/godotenv"
	omise "github.com/omise/omise-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/a2n2k3p4/tutorium-backend/handlers"
	"github.com/a2n2k3p4/tutorium-backend/models"
)

func main() {
	_ = godotenv.Load()

	// Database connection
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=disable",
		os.Getenv("DB_HOST"),
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASSWORD"),
		os.Getenv("DB_NAME"),
		os.Getenv("DB_PORT"),
	)

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto migrate models
	if err := db.AutoMigrate(&models.User{}, &models.Transaction{}); err != nil {
		log.Fatal("Failed to migrate database:", err)
	}

	// Omise client setup
	publicKey := os.Getenv("OMISE_PUBLIC_KEY")
	secretKey := os.Getenv("OMISE_SECRET_KEY")
	if publicKey == "" || secretKey == "" {
		log.Fatal("OMISE_PUBLIC_KEY and OMISE_SECRET_KEY must be set")
	}

	client, err := omise.NewClient(publicKey, secretKey)
	if err != nil {
		log.Fatal("Failed to create Omise client:", err)
	}

	// Initialize handlers
	paymentHandler := handlers.NewPaymentHandler(db, client)

	// Create Fiber app
	app := fiber.New()

	// Middleware (Cors) TODO: integrate middleware into transaction handlers, or use CORS idc
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET, POST, PUT, DELETE, OPTIONS",
		AllowHeaders: "Content-Type, Authorization, X-User-ID",
	}))

	// Routes
	app.Get("/health", paymentHandler.Health)
	app.Post("/payments/charge", paymentHandler.CreateCharge)
	app.Get("/payments/transactions", paymentHandler.ListTransactions)
	app.Get("/payments/transactions/:id", paymentHandler.GetTransaction)
	app.Post("/webhooks/omise", paymentHandler.HandleWebhook)

	fmt.Println("Server running on http://localhost:8080")
	log.Fatal(app.Listen(":8080"))
}
