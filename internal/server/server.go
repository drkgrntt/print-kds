package server

import (
	"print-kds/internal/auth"
	"print-kds/internal/board"
	"print-kds/internal/database"

	"github.com/gofiber/fiber/v2"
)

type FiberServer struct {
	*fiber.App

	db         database.Service
	boardStore *board.Store
	authStore  *auth.Store
}

func New() *FiberServer {
	db := database.New()

	server := &FiberServer{
		App: fiber.New(fiber.Config{
			ServerHeader: "print-kds",
			AppName:      "print-kds",
		}),

		db:         db,
		boardStore: board.NewStore(db.DB()),
		authStore:  auth.NewStore(db.DB()),
	}

	return server
}
