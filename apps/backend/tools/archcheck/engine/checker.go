// Package engine enforces the dependency rules from the architecture
// document: the layer boundaries of every module, the ban on cross-module
// repository/handler/service imports, the purity of domain packages, and the
// rule that platform holds infrastructure without business logic.
//
// It is run by `make archcheck` and by CI, which fails the pull request on any
// violation.
package engine

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Layer names inside a module.
const (
	LayerDomain     = "domain"
	LayerRepository = "repository"
	LayerService    = "service"
	LayerHandler    = "handler"
	// LayerActivity is the Temporal activity layer of the task module. It talks
	// to the outside world, so it is bounded like a service rather than trusted
	// like the workflow next to it.
	LayerActivity = "activity"
)

// Violation is one broken rule.
type Violation struct {
	File    string
	Line    int
	Package string
	Import  string
	Rule    string
	Message string
}

// String renders a violation as a compiler-style line so CI output points at
// the offending file.
func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: [%s] %s imports %s: %s", v.File, v.Line, v.Rule, v.Package, v.Import, v.Message)
}

// Import is a single import statement of a package.
type Import struct {
	Path string
	File string
	Line int
}

// Package is one Go package of the module.
type Package struct {
	// ImportPath is the full import path, e.g.
	// github.com/robzlabz/employeebot/apps/backend/internal/modules/health/domain
	ImportPath string
	// Rel is the path relative to the module root, e.g. internal/modules/health/domain
	Rel string
	// Imports are the direct imports of the package.
	Imports []Import
}

// location describes where a package sits in the layout.
type location struct {
	kind       string // module | platform | container | cmd | tools | other
	module     string // module name, when kind == module
	layer      string // domain | repository | service | handler, when kind == module
	platform   string // platform sub-package, when kind == platform
	isInternal bool
}

// Check reports every dependency-rule violation in pkgs. The module prefix is
// the import path of the Go module under check.
func Check(modulePrefix string, pkgs []Package) []Violation {
	var violations []Violation

	for _, pkg := range pkgs {
		from := locate(modulePrefix, pkg.Rel)
		if from.kind == "other" || from.kind == "tools" || from.kind == "generated" {
			continue
		}

		for _, imp := range pkg.Imports {
			to, isInternalImport := locateImport(modulePrefix, imp.Path)

			if isInternalImport {
				if v, ok := checkInternal(from, to, pkg, imp); ok {
					violations = append(violations, v)
				}
				continue
			}

			if v, ok := checkExternal(from, pkg, imp); ok {
				violations = append(violations, v)
			}
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})

	return violations
}

func checkInternal(from, to location, pkg Package, imp Import) (Violation, bool) {
	violation := Violation{
		File:    imp.File,
		Line:    imp.Line,
		Package: pkg.Rel,
		Import:  imp.Path,
	}

	switch from.kind {
	case "module":
		switch to.kind {
		case "module":
			if to.module == from.module {
				// Within a module, only the layer order domain ← repository ←
				// service ← handler is allowed, and handler never touches
				// repository directly.
				if from.layer == LayerHandler && to.layer == LayerRepository {
					violation.Rule = "handler-no-repository"
					violation.Message = "handler must call the service interface, never a repository"
					return violation, true
				}
				if from.layer == LayerDomain && to.layer != LayerDomain {
					violation.Rule = "domain-isolated"
					violation.Message = "domain must not import another layer of its own module"
					return violation, true
				}
				return Violation{}, false
			}

			// Cross-module: only domain contracts may be shared.
			if to.layer != LayerDomain {
				violation.Rule = "cross-module-boundary"
				violation.Message = "modules may only import another module's domain package"
				return violation, true
			}
			return Violation{}, false

		case "platform":
			return checkModuleToPlatform(from, to, violation)

		case "container":
			violation.Rule = "no-container-import"
			violation.Message = "the container wires modules; modules never import it"
			return violation, true
		}

	case "platform":
		if to.kind == "module" {
			// An adapter implements a port its domain declares, which is the
			// correct direction: infrastructure depends on the contract, never
			// the other way round. Anything past the domain — a repository, a
			// service, a handler — is still forbidden, because that would drag
			// business logic into platform and invite cycles.
			if to.layer == LayerDomain {
				return Violation{}, false
			}
			violation.Rule = "platform-imports-module"
			violation.Message = "platform may only import a module's domain package to implement its port"
			return violation, true
		}
		if to.kind == "container" {
			violation.Rule = "no-container-import"
			violation.Message = "platform must not import the container"
			return violation, true
		}

	case "cmd":
		if to.kind == "module" {
			violation.Rule = "cmd-through-container"
			violation.Message = "cmd must reach modules through the container"
			return violation, true
		}
	}

	return Violation{}, false
}

// checkModuleToPlatform limits which platform packages a layer may use.
//
// attachtext is the reading of a message's attachments: a service renders them
// into the prompt and a task activity resolves them into the turns it sends, and
// both do it through this one package so a file is read the same way whichever
// path a question arrives on.
func checkModuleToPlatform(from, to location, violation Violation) (Violation, bool) {
	allowed := map[string]map[string]bool{
		LayerDomain:     {},
		LayerRepository: {"database": true, "redis": true},
		LayerService:    {"logger": true, "config": true, "attachtext": true},
		LayerActivity:   {"attachtext": true},
		LayerHandler:    {"logger": true, "middleware": true, "response": true},
	}

	if allowed[from.layer][to.platform] {
		return Violation{}, false
	}

	violation.Rule = "layer-platform-access"
	violation.Message = fmt.Sprintf("%s may not import platform/%s", from.layer, to.platform)
	return violation, true
}

// checkExternal forbids third-party imports in the innermost layers. Standard
// library packages (no dot in the first path segment) are always allowed.
func checkExternal(from location, pkg Package, imp Import) (Violation, bool) {
	if from.kind != "module" || isStandardLibrary(imp.Path) {
		return Violation{}, false
	}

	violation := Violation{
		File:    imp.File,
		Line:    imp.Line,
		Package: pkg.Rel,
		Import:  imp.Path,
	}

	switch from.layer {
	case LayerDomain:
		// The architecture document requires domain to be free of any internal
		// package and of the frameworks and drivers it names (Fiber, pgx, Zap,
		// sqlc). Plain value types such as a UUID library are allowed: entities
		// need an identifier type.
		if isInfrastructureLibrary(imp.Path) {
			violation.Rule = "domain-pure"
			violation.Message = "domain must not import a framework or driver (Fiber, pgx, Redis, Temporal, Zap, sqlc)"
			return violation, true
		}
		return Violation{}, false
	case LayerService:
		// Services may use logging and generic helpers, nothing that binds
		// them to a transport or a driver.
		if isForbiddenInService(imp.Path) {
			violation.Rule = "service-transport-free"
			violation.Message = "service must not import a transport or database driver"
			return violation, true
		}
	}

	return Violation{}, false
}

// infrastructureLibraries are the frameworks, drivers, and generated clients a
// domain package must never depend on.
var infrastructureLibraries = []string{
	"github.com/gofiber/",
	"github.com/jackc/pgx/",
	"github.com/redis/go-redis/",
	"go.temporal.io/",
	"go.uber.org/zap",
	"database/sql",
	"net/http",
	"net/rpc",
	"/sqlcgen",
}

func isInfrastructureLibrary(importPath string) bool {
	for _, prefix := range infrastructureLibraries {
		if importPath == prefix || strings.HasPrefix(importPath, prefix) || strings.HasSuffix(importPath, prefix) {
			return true
		}
	}
	return false
}

var forbiddenInService = []string{
	"github.com/gofiber/",
	"github.com/jackc/pgx/",
	"github.com/redis/go-redis/",
	"go.temporal.io/sdk/client",
	"go.temporal.io/sdk/worker",
	"database/sql",
}

func isForbiddenInService(importPath string) bool {
	for _, prefix := range forbiddenInService {
		if importPath == prefix || strings.HasPrefix(importPath, prefix) {
			return true
		}
	}
	return false
}

// isStandardLibrary reports whether an import path is a standard library
// package: standard library paths never contain a dot in their first segment.
func isStandardLibrary(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// generatedDirs hold code produced by a generator (sqlc, mockery). Rules are
// not applied *to* a generated package — it is re-created from its source and
// never hand-edited — but importing one counts as importing the layer it
// belongs to, so `domain` importing `repository/sqlcgen` is still a violation.
var generatedDirs = map[string]bool{"sqlcgen": true, "mocks": true}

// locate maps a module-relative path to its place in the layout.
func locate(modulePrefix, rel string) location {
	segments := strings.Split(rel, "/")
	if len(segments) == 0 {
		return location{kind: "other"}
	}
	for _, segment := range segments {
		if generatedDirs[segment] {
			return location{kind: "generated"}
		}
	}

	switch segments[0] {
	case "cmd":
		return location{kind: "cmd"}
	case "tools", "migrations":
		return location{kind: "tools"}
	case "internal":
		return locateInternal(segments)
	default:
		return location{kind: "other"}
	}
}

func locateInternal(segments []string) location {
	if len(segments) < 2 {
		return location{kind: "other"}
	}

	switch segments[1] {
	case "modules":
		if len(segments) < 4 {
			return location{kind: "other"}
		}
		return location{
			kind:       "module",
			module:     segments[2],
			layer:      segments[3],
			isInternal: true,
		}
	case "platform":
		if len(segments) < 3 {
			return location{kind: "platform"}
		}
		return location{kind: "platform", platform: segments[2], isInternal: true}
	case "container":
		return location{kind: "container", isInternal: true}
	default:
		return location{kind: "other"}
	}
}

// locateImport maps an import path to its place in the layout, reporting
// whether the import points inside this module at all.
func locateImport(modulePrefix, importPath string) (location, bool) {
	if !strings.HasPrefix(importPath, modulePrefix+"/") {
		return location{}, false
	}
	rel := strings.TrimPrefix(importPath, modulePrefix+"/")
	return locateTarget(path.Clean(rel)), true
}

// locateTarget maps an import target to the layer it belongs to. A generated
// package resolves to its parent layer, so importing generated query code counts
// as importing the repository.
func locateTarget(rel string) location {
	segments := strings.Split(rel, "/")
	trimmed := make([]string, 0, len(segments))
	for _, segment := range segments {
		if generatedDirs[segment] {
			continue
		}
		trimmed = append(trimmed, segment)
	}
	return locate("", strings.Join(trimmed, "/"))
}
