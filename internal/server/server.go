package server

import (
	"os"
	"strconv"

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
	uploadDir  string
}

const (
	defaultUploadDir   = "./data/uploads"
	defaultMaxUploadMB = 100
)

// maxUploadBytes reads MAX_UPLOAD_MB (default 100) - the request body
// limit, which has to cover a whole order's worth of model files.
func maxUploadBytes() int {
	mb := defaultMaxUploadMB
	if v, err := strconv.Atoi(os.Getenv("MAX_UPLOAD_MB")); err == nil && v > 0 {
		mb = v
	}
	return mb * 1024 * 1024
}

func New() *FiberServer {
	db := database.New()

	server := &FiberServer{
		App: fiber.New(fiber.Config{
			ServerHeader: "print-kds",
			AppName:      "print-kds",
			BodyLimit:    maxUploadBytes(),
		}),

		db:         db,
		boardStore: board.NewStore(db.DB()),
		authStore:  auth.NewStore(db.DB()),
		uploadDir:  uploadDirFromEnv(),
	}

	return server
}

func uploadDirFromEnv() string {
	if v := os.Getenv("UPLOAD_DIR"); v != "" {
		return v
	}
	return defaultUploadDir
}
