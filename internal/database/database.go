package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	"print-kds/internal/logger"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/joho/godotenv/autoload"
)

// Service represents a service that interacts with a database.
type Service interface {
	// Health returns a map of health status information.
	Health(ctx context.Context) map[string]string

	// Close terminates the database connection.
	Close() error

	// DB returns the underlying connection pool, for packages that need to
	// run their own queries against it.
	DB() *sql.DB
}

type service struct {
	db *sql.DB
}

var dbInstance *service

// connString reads DB_* from the environment at call time (rather than
// into package vars at init time) so that anything which sets them late -
// e.g. a test loading .env from a path relative to its own source file,
// since godotenv/autoload's own init() only ever looks in the process's
// starting working directory - still gets picked up correctly by New().
func connString() string {
	schema := os.Getenv("DB_SCHEMA")
	if schema == "" {
		schema = "public"
	}

	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable&search_path=%s",
		os.Getenv("DB_USERNAME"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"), os.Getenv("DB_DATABASE"), schema,
	)
}

func New() Service {
	// Reuse Connection
	if dbInstance != nil {
		return dbInstance
	}

	db, err := sql.Open("pgx", connString())
	if err != nil {
		// This will not be a connection error, but a DSN parse error or
		// another initialization error.
		logger.Fatal("open database", "error", err)
	}

	if err := migrate(db); err != nil {
		logger.Fatal("run migrations", "error", err)
	}

	dbInstance = &service{
		db: db,
	}
	return dbInstance
}

// DB returns the underlying connection pool.
func (s *service) DB() *sql.DB {
	return s.db
}

// Health checks the health of the database connection by pinging the database.
func (s *service) Health(ctx context.Context) map[string]string {
	pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	stats := make(map[string]string)

	err := s.db.PingContext(pingCtx)
	if err != nil {
		stats["status"] = "down"
		stats["error"] = fmt.Sprintf("db down: %v", err)
		logger.Error("db down", "error", err)
		return stats
	}

	stats["status"] = "up"
	stats["message"] = "It's healthy"

	dbStats := s.db.Stats()
	stats["open_connections"] = strconv.Itoa(dbStats.OpenConnections)
	stats["in_use"] = strconv.Itoa(dbStats.InUse)
	stats["idle"] = strconv.Itoa(dbStats.Idle)

	return stats
}

// Close closes the database connection.
func (s *service) Close() error {
	logger.Info("disconnected from database", "database", os.Getenv("DB_DATABASE"))
	return s.db.Close()
}
