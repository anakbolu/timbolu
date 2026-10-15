// Command migrate applies, reverts, or reports the schema version using the
// migrations embedded in the binary.
//
// It exists so CI and deployments do not need the golang-migrate CLI, and so
// both directions are exercised by the same code path the tests use.
//
// Usage: go run ./tools/migrate [up|down|version] [-url postgres://...]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	url := flag.String("url", "", "database url (defaults to $DATABASE_URL)")
	flag.Parse()

	command := "up"
	if args := flag.Args(); len(args) > 0 {
		command = strings.ToLower(args[0])
	}

	target := *url
	if target == "" {
		target = os.Getenv("DATABASE_URL")
	}
	if target == "" {
		return fmt.Errorf("no database url: pass -url or set DATABASE_URL")
	}

	ctx := context.Background()

	switch command {
	case "up":
		if err := migrate.MigrateUp(ctx, target); err != nil {
			return err
		}
		fmt.Println("migrate: up applied")
		return report(ctx, target)
	case "down":
		if err := migrate.MigrateDown(ctx, target); err != nil {
			return err
		}
		fmt.Println("migrate: down reverted")
		return report(ctx, target)
	case "version":
		return report(ctx, target)
	default:
		return fmt.Errorf("unknown command %q: use up, down, or version", command)
	}
}

func report(ctx context.Context, url string) error {
	version, dirty, err := migrate.MigrateVersion(ctx, url)
	if err != nil {
		return err
	}
	if version == 0 {
		fmt.Println("migrate: schema is empty")
		return nil
	}
	fmt.Printf("migrate: version %d (dirty=%t)\n", version, dirty)
	return nil
}
