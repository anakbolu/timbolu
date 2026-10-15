package config

import (
	"errors"
	"os"

	"github.com/joho/godotenv"
)

// LoadDotEnv loads a .env file when one is present next to the working
// directory. A missing file is not an error: docker-compose and CI supply the
// variables directly.
func LoadDotEnv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return err
		}
	}
	return nil
}
