// Command archcheck fails the build when a package breaks the dependency rules
// of the modular monolith (see docs/testing.md and the architecture document).
//
// Usage: go run ./tools/archcheck [-root .]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/anakbolu/timbolu/apps/backend/tools/archcheck/engine"
)

func main() {
	root := flag.String("root", ".", "module root directory")
	flag.Parse()

	packages, err := engine.Scan(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archcheck: %v\n", err)
		os.Exit(2)
	}

	modulePrefix, err := engine.ModulePath(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archcheck: %v\n", err)
		os.Exit(2)
	}

	violations := engine.Check(modulePrefix, packages)
	if len(violations) == 0 {
		fmt.Printf("archcheck: %d packages checked, no violations\n", len(packages))
		return
	}

	for _, violation := range violations {
		fmt.Fprintln(os.Stderr, violation.String())
	}
	fmt.Fprintf(os.Stderr, "archcheck: %d violation(s)\n", len(violations))
	os.Exit(1)
}
