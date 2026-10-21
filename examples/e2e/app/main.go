package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
		if os.Getenv("HEALTH_FAIL") == "1" {
			http.Error(w, "unhealthy", http.StatusInternalServerError)
			return
		}
		greeting := os.Getenv("GREETING")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok %s", greeting)
	})
	// The E2E checks which client address Traefik forwards.
	http.HandleFunc("/echo-xff", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Forwarded-For"))
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
	if url := os.Getenv("DATABASE_URL"); url != "" {
		sql := "create table if not exists items (id serial primary key, name text not null)"
		if out, err := exec.Command("psql", url, "-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "migration failed: %v\n%s", err, out)
			os.Exit(1)
		}
	}
	fmt.Println("migrated")
	os.Exit(0)
}
