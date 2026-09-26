// Command dbreset wipes the local database and rebuilds it from the
// migrations in internal/database/migrations, so a broken or messy local
// dev database can be thrown away without reaching for psql by hand.
//
// Run it with:
//
//	go run ./cmd/dbreset
//
// or, to skip the confirmation prompt (e.g. from another script):
//
//	go run ./cmd/dbreset -yes
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"print-kds/internal/database"
	"print-kds/internal/logger"

	_ "github.com/joho/godotenv/autoload"
)

func main() {
	skipConfirm := flag.Bool("yes", false, "skip the confirmation prompt")
	flag.Parse()

	if !*skipConfirm && !confirm() {
		fmt.Println("Aborted - nothing was changed.")
		os.Exit(1)
	}

	db, err := database.Open()
	if err != nil {
		logger.Fatal("connect to database", "error", err)
	}
	defer db.Close()

	fmt.Println("Dropping every table and re-running migrations from scratch...")
	if err := database.Reset(db); err != nil {
		logger.Fatal("reset database", "error", err)
	}

	fmt.Println("Database reset complete.")
}

func confirm() bool {
	fmt.Printf("This will permanently delete ALL data in %s@%s:%s/%s and rebuild an empty schema. Type \"reset\" to continue: ",
		os.Getenv("DB_USERNAME"), os.Getenv("DB_HOST"), os.Getenv("DB_PORT"), os.Getenv("DB_DATABASE"))

	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(answer) == "reset"
}
