package engine

import "testing"

const testModule = "github.com/example/bolu/apps/backend"

func pkg(rel string, imports ...string) Package {
	importsOf := make([]Import, 0, len(imports))
	for _, path := range imports {
		importsOf = append(importsOf, Import{Path: path, File: rel + "/file.go", Line: 3})
	}
	return Package{ImportPath: testModule + "/" + rel, Rel: rel, Imports: importsOf}
}

func internal(rel string) string { return testModule + "/" + rel }

func TestCheck(t *testing.T) {
	tests := []struct {
		name     string
		packages []Package
		wantRule string // empty means no violation expected
	}{
		{
			name: "clean layout has no violations",
			packages: []Package{
				pkg("internal/modules/health/domain", "context", "github.com/google/uuid"),
				pkg("internal/modules/health/repository", internal("internal/modules/health/domain"), internal("internal/platform/database"), "github.com/jackc/pgx/v5"),
				pkg("internal/modules/health/service", internal("internal/modules/health/domain"), internal("internal/platform/logger")),
				pkg("internal/modules/health/handler", internal("internal/modules/health/domain"), internal("internal/platform/response"), "github.com/gofiber/fiber/v2"),
				pkg("internal/container", internal("internal/modules/health/handler"), internal("internal/modules/health/repository")),
				pkg("internal/platform/database", "github.com/jackc/pgx/v5/pgxpool"),
				pkg("cmd/api", internal("internal/container"), internal("internal/platform/config")),
				pkg("internal/modules/health/domain/mocks", "github.com/stretchr/testify/mock"),
				pkg("internal/modules/health/repository/sqlcgen", "github.com/jackc/pgx/v5"),
				pkg("tools/archcheck", "go/parser"),
			},
		},
		{
			name: "domain must not import a framework",
			packages: []Package{
				pkg("internal/modules/health/domain", "github.com/gofiber/fiber/v2"),
			},
			wantRule: "domain-pure",
		},
		{
			name: "domain must not import a driver",
			packages: []Package{
				pkg("internal/modules/health/domain", "github.com/jackc/pgx/v5/pgxpool"),
			},
			wantRule: "domain-pure",
		},
		{
			name: "domain must not import generated query code",
			packages: []Package{
				pkg("internal/modules/health/domain", internal("internal/modules/health/repository/sqlcgen")),
			},
			wantRule: "domain-isolated",
		},
		{
			name: "repository may import its generated query code",
			packages: []Package{
				pkg("internal/modules/health/repository", internal("internal/modules/health/repository/sqlcgen"), internal("internal/modules/health/domain")),
			},
		},
		{
			name: "domain may use plain value types",
			packages: []Package{
				pkg("internal/modules/health/domain", "github.com/google/uuid", "time", "context", "errors"),
			},
		},
		{
			name: "domain must not import platform",
			packages: []Package{
				pkg("internal/modules/health/domain", internal("internal/platform/logger")),
			},
			wantRule: "layer-platform-access",
		},
		{
			name: "domain must not import another layer of its own module",
			packages: []Package{
				pkg("internal/modules/health/domain", internal("internal/modules/health/service")),
			},
			wantRule: "domain-isolated",
		},
		{
			name: "handler must not reach a repository",
			packages: []Package{
				pkg("internal/modules/health/handler", internal("internal/modules/health/repository")),
			},
			wantRule: "handler-no-repository",
		},
		{
			name: "modules may only share domain contracts",
			packages: []Package{
				pkg("internal/modules/agent/service", internal("internal/modules/knowledge/repository")),
			},
			wantRule: "cross-module-boundary",
		},
		{
			name: "module service may import another module's domain",
			packages: []Package{
				pkg("internal/modules/agent/service", internal("internal/modules/knowledge/domain")),
			},
		},
		{
			name: "service must stay transport free",
			packages: []Package{
				pkg("internal/modules/agent/service", "github.com/jackc/pgx/v5/pgxpool"),
			},
			wantRule: "service-transport-free",
		},
		{
			name: "repository may only use data platform packages",
			packages: []Package{
				pkg("internal/modules/agent/repository", internal("internal/platform/logger")),
			},
			wantRule: "layer-platform-access",
		},
		{
			name: "platform has no business logic",
			packages: []Package{
				pkg("internal/platform/database", internal("internal/modules/agent/repository")),
			},
			wantRule: "platform-imports-module",
		},
		{
			name: "platform may implement a domain port",
			packages: []Package{
				pkg("internal/platform/llm/openai", internal("internal/modules/llm/domain"), "net/http"),
			},
		},
		{
			name: "platform may not import a module service",
			packages: []Package{
				pkg("internal/platform/llm/openai", internal("internal/modules/llm/service")),
			},
			wantRule: "platform-imports-module",
		},
		{
			name: "modules never import the container",
			packages: []Package{
				pkg("internal/modules/agent/service", internal("internal/container")),
			},
			wantRule: "no-container-import",
		},
		{
			name: "service may read attachments",
			packages: []Package{
				pkg("internal/modules/chat/service", internal("internal/platform/attachtext"), internal("internal/platform/logger")),
			},
		},
		{
			name: "task activity may read attachments",
			packages: []Package{
				pkg("internal/modules/task/activity", internal("internal/platform/attachtext")),
			},
		},
		{
			name: "attachment reading is the only platform package an activity gets",
			packages: []Package{
				pkg("internal/modules/task/activity", internal("internal/platform/database")),
			},
			wantRule: "layer-platform-access",
		},
		{
			name: "a handler may not read attachments",
			packages: []Package{
				pkg("internal/modules/chat/handler", internal("internal/platform/attachtext")),
			},
			wantRule: "layer-platform-access",
		},
		{
			name: "cmd reaches modules through the container",
			packages: []Package{
				pkg("cmd/api", internal("internal/modules/agent/handler")),
			},
			wantRule: "cmd-through-container",
		},
		{
			name: "generated packages are exempt",
			packages: []Package{
				pkg("internal/modules/agent/domain/mocks", "github.com/gofiber/fiber/v2"),
				pkg("internal/modules/agent/repository/sqlcgen", "github.com/jackc/pgx/v5"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := Check(testModule, tt.packages)

			if tt.wantRule == "" {
				if len(violations) != 0 {
					t.Fatalf("expected no violations, got %v", violations)
				}
				return
			}

			if len(violations) == 0 {
				t.Fatalf("expected a %s violation, got none", tt.wantRule)
			}
			if violations[0].Rule != tt.wantRule {
				t.Fatalf("expected rule %s, got %s (%v)", tt.wantRule, violations[0].Rule, violations[0])
			}
		})
	}
}

func TestViolationStringPointsAtTheFile(t *testing.T) {
	violations := Check(testModule, []Package{
		pkg("internal/modules/health/domain", "github.com/gofiber/fiber/v2"),
	})
	if len(violations) != 1 {
		t.Fatalf("expected one violation, got %d", len(violations))
	}

	got := violations[0].String()
	want := "internal/modules/health/domain/file.go:3: [domain-pure]"
	if len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("violation string %q does not start with %q", got, want)
	}
}
