package contract_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

func TestCheckFieldsForbidden(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantService string
		wantPath    string
	}{
		{
			name: "privileged",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    privileged: true
`,
			wantService: "api",
			wantPath:    "services.api.privileged",
		},
		{
			name: "volumes bind",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    volumes:
      - /host:/container
`,
			wantService: "api",
			wantPath:    "services.api.volumes",
		},
		{
			name: "network_mode",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    network_mode: host
`,
			wantService: "api",
			wantPath:    "services.api.network_mode",
		},
		{
			name: "env_file",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    env_file:
      - .env
`,
			wantService: "api",
			wantPath:    "services.api.env_file",
		},
		{
			name: "top-level networks",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
networks:
  custom: {}
`,
			wantService: "",
			wantPath:    "networks",
		},
		{
			name: "top-level volumes",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
volumes:
  data: {}
`,
			wantService: "",
			wantPath:    "volumes",
		},
		{
			name: "top-level include",
			raw: `
include:
  - other.yml
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
`,
			wantService: "",
			wantPath:    "include",
		},
		{
			name: "extends",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    extends:
      file: common.yml
      service: base
`,
			wantService: "api",
			wantPath:    "services.api.extends",
		},
		{
			name: "build",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    build: .
`,
			wantService: "api",
			wantPath:    "services.api.build",
		},
		{
			name: "cap_add",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    cap_add:
      - SYS_ADMIN
`,
			wantService: "api",
			wantPath:    "services.api.cap_add",
		},
		{
			name: "user",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    user: "1000:1000"
`,
			wantService: "api",
			wantPath:    "services.api.user",
		},
		{
			name: "devices",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    devices:
      - /dev/ttyUSB0
`,
			wantService: "api",
			wantPath:    "services.api.devices",
		},
		{
			name: "service secrets",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    secrets:
      - my_secret
`,
			wantService: "api",
			wantPath:    "services.api.secrets",
		},
		{
			name: "top-level secrets",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
secrets:
  my_secret:
    file: ./secret.txt
`,
			wantService: "",
			wantPath:    "secrets",
		},
		{
			name: "service configs",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    configs:
      - my_config
`,
			wantService: "api",
			wantPath:    "services.api.configs",
		},
		{
			name: "top-level configs",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
configs:
  my_config:
    file: ./config.txt
`,
			wantService: "",
			wantPath:    "configs",
		},
		{
			name: "top-level x-foo",
			raw: `
x-foo:
  bar: baz
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
`,
			wantService: "",
			wantPath:    "x-foo",
		},
		{
			name: "traefik label in map",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
      traefik.http.routers.api.rule: Host("api.example.com")
`,
			wantService: "api",
			wantPath:    "services.api.labels.traefik.http.routers.api.rule",
		},
		{
			name: "traefik label in list",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      - cvctl.role=app
      - traefik.enable=true
`,
			wantService: "api",
			wantPath:    "services.api.labels.traefik.enable",
		},
		{
			name: "data service with image",
			raw: `
services:
  db:
    image: postgres:16
    labels:
      cvctl.kind: postgres
`,
			wantService: "db",
			wantPath:    "services.db.image",
		},
		{
			name: "data service with environment",
			raw: `
services:
  db:
    labels:
      cvctl.kind: postgres
    environment:
      POSTGRES_PASSWORD: secret
`,
			wantService: "db",
			wantPath:    "services.db.environment",
		},
		{
			name: "deploy placement",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    deploy:
      placement:
        constraints:
          - node.role == manager
`,
			wantService: "api",
			wantPath:    "services.api.deploy.placement",
		},
		{
			name: "deploy resources reservations pids",
			raw: `
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
    deploy:
      resources:
        reservations:
          pids: 100
`,
			wantService: "api",
			wantPath:    "services.api.deploy.resources.reservations.pids",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issues := contract.CheckFields([]byte(tc.raw))
			if len(issues) == 0 {
				t.Fatalf("expected forbidden field issue for %q, got 0 issues", tc.name)
			}
			found := false
			for _, is := range issues {
				if is.Code != "E_CONTRACT_FIELD_FORBIDDEN" {
					t.Errorf("expected code E_CONTRACT_FIELD_FORBIDDEN, got %q", is.Code)
				}
				if is.Service == tc.wantService && is.Message == tc.wantPath {
					found = true
					wantHint := fmt.Sprintf("remove %s; cvctl renders networks, volumes and security options itself", tc.wantPath)
					if is.Hint != wantHint {
						t.Errorf("hint mismatch: got %q, want %q", is.Hint, wantHint)
					}
				}
			}
			if !found {
				t.Fatalf("issue for path %q service %q not found in %+v", tc.wantPath, tc.wantService, issues)
			}
		})
	}
}

func TestCheckFieldsAllowed(t *testing.T) {
	raw := `
x-cvctl:
  health:
    service: api
    http: /health
    timeout: 30s
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
      app.version: "1.0.0"
    environment:
      ENV_VAR: "value"
    command: ["run", "server"]
    entrypoint: ["/entrypoint.sh"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/health"]
      interval: 10s
    deploy:
      resources:
        limits:
          memory: 256m
          cpus: "1.0"
        reservations:
          memory: 128m
          cpus: "0.5"
    stop_grace_period: 30s
    depends_on:
      - worker
  worker:
    image: ghcr.io/org/worker:1
    labels:
      cvctl.role: worker
    deploy:
      resources:
        limits:
          memory: 128m
  db:
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: DATABASE
    deploy:
      resources:
        limits:
          memory: 512m
`
	issues := contract.CheckFields([]byte(raw))
	if len(issues) != 0 {
		t.Fatalf("expected 0 issues for allowed compose, got: %+v", issues)
	}
}

func TestLoadNeverReachesLoaderWhenIssuesExist(t *testing.T) {
	dir := t.TempDir()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	// compose.yml has top-level include pointing to a missing file.
	// If loader ran, it would fail with a file error or loader syntax error.
	// But CheckFields must catch top-level include first and return *output.IssuesError.
	composeContent := `
include:
  - this-file-does-not-exist.yml
services:
  api:
    image: ghcr.io/org/api:1
    labels:
      cvctl.role: app
`
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(composeContent), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = contract.Load(dir, cat)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ie *output.IssuesError
	if !errors.As(err, &ie) {
		t.Fatalf("expected *output.IssuesError, got %T: %v", err, err)
	}
	if len(ie.Issues) == 0 || ie.Issues[0].Code != "E_CONTRACT_FIELD_FORBIDDEN" {
		t.Fatalf("expected E_CONTRACT_FIELD_FORBIDDEN in IssuesError, got %+v", ie.Issues)
	}
}

func TestCheckFieldsRejectsYAMLIndirection(t *testing.T) {
	cases := map[string]string{
		"alias service": `x-cvctl:
  t: &a {image: a, privileged: true, labels: {cvctl.role: app}}
services:
  api: *a
`,
		"merge key": `services:
  api:
    <<: {privileged: true}
    image: a
    labels: {cvctl.role: app}
`,
		"anchored labels": `services:
  api:
    image: a
    labels: &l {cvctl.role: app}
  web:
    image: b
    labels: *l
`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			issues := contract.CheckFields([]byte(raw))
			found := false
			for _, is := range issues {
				if is.Code == "E_CONTRACT_FIELD_FORBIDDEN" && strings.Contains(is.Message, "not allowed") {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected an anchor/alias/merge issue, got %v", issues)
			}
		})
	}
}

func TestCheckFieldsPortsAndShapes(t *testing.T) {
	issues := contract.CheckFields([]byte(`services:
  api:
    image: a
    labels: {cvctl.role: app}
    ports: ["80:80"]
  db:
    labels: {cvctl.kind: postgres}
    deploy: 512m
`))
	codes := map[string]bool{}
	for _, is := range issues {
		codes[is.Code+" "+is.Message] = true
	}
	if !codes["E_CONTRACT_PORTS_PUBLISHED service publishes host ports"] {
		t.Fatalf("ports not rejected: %v", issues)
	}
	if !codes["E_CONTRACT_FIELD_FORBIDDEN services.db.deploy"] {
		t.Fatalf("non-mapping deploy not rejected: %v", issues)
	}
}
