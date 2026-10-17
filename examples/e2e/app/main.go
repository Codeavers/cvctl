package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		runServe()
	case "migrate":
		runMigrate()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(2)
	}
}

func runServe() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		greeting := os.Getenv("GREETING")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok %s", greeting)
	})

	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func runMigrate() {
	if os.Getenv("MIGRATE_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "migration failed")
		os.Exit(1)
	}
	fmt.Println("migrated")
	os.Exit(0)
}
