// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/postgressetup"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestHelmPostgresConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		values                                                     []string
		wantReadWrite, wantOwner, wantReadWriteRole, wantOwnerRole string
		wantError                                                  bool
	}{
		{name: "bundled", wantReadWriteRole: postgressetup.ReadWriteRole, wantOwnerRole: postgressetup.OwnerRole},
		{name: "bundled custom roles", values: []string{"postgres.readWriteRole=runtime", "postgres.ownerRole=owner", "postgres.schema=custom_schema"}, wantReadWriteRole: "runtime", wantOwnerRole: "owner"},
		{name: "external", values: []string{"postgres.enabled=false", "postgres.readWriteConnectionString=postgresql://runtime@db/atepg", "postgres.ownerConnectionString=postgresql://owner@db/atepg", "postgres.readWriteRole=runtime", "postgres.ownerRole=owner"}, wantReadWrite: "postgresql://runtime@db/atepg", wantOwner: "postgresql://owner@db/atepg", wantReadWriteRole: "runtime", wantOwnerRole: "owner"},
		{name: "shared login", values: []string{"postgres.enabled=false", "postgres.readWriteConnectionString=postgresql://login@db/atepg", "postgres.readWriteRole=runtime", "postgres.ownerRole=owner"}, wantReadWrite: "postgresql://login@db/atepg", wantOwner: "postgresql://login@db/atepg", wantReadWriteRole: "runtime", wantOwnerRole: "owner"},
		{name: "missing external connection", values: []string{"postgres.enabled=false"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", "test", "../../charts/substrate", "-n", "custom"}
			for _, value := range tc.values {
				args = append(args, "--set", value)
			}
			out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
			if tc.wantError {
				if err == nil || !strings.Contains(string(out), "postgres.readWriteConnectionString is required") {
					t.Fatalf("expected missing external connection error: %v\n%s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			var env map[string]string
			var postgresConfig map[string]string
			var postgres *corev1.Container
			var container *corev1.Container
			for _, doc := range strings.Split(string(out), "\n---\n") {
				var cm corev1.ConfigMap
				if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
					t.Fatal(err)
				}
				if cm.Kind == "ConfigMap" && cm.Name == "ate-api-server-envvars" {
					env = cm.Data
				}
				if cm.Kind == "ConfigMap" && cm.Name == "test-postgres-config" {
					postgresConfig = cm.Data
				}
				if cm.Kind == "StatefulSet" && cm.Name == "test-postgres" {
					var statefulSet appsv1.StatefulSet
					if err := yaml.Unmarshal([]byte(doc), &statefulSet); err != nil {
						t.Fatal(err)
					}
					for _, c := range statefulSet.Spec.Template.Spec.Containers {
						if c.Name == "postgres" {
							postgres = &c
						}
					}
				}
				if cm.Kind != "Deployment" {
					continue
				}
				var deployment appsv1.Deployment
				if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
					t.Fatal(err)
				}
				for _, c := range deployment.Spec.Template.Spec.Containers {
					if c.Name == "ate-api-server" {
						container = &c
					}
				}
			}
			if env == nil || container == nil {
				t.Fatal("missing API server configuration")
			}
			for _, arg := range container.Args {
				name, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
				if pflag.Lookup(name) == nil {
					t.Errorf("chart passes unknown ateapi flag %q", name)
				}
			}
			if tc.wantReadWrite == "" {
				const endpoint = "@test-postgres.custom.svc:5432/atepg?sslmode=verify-full&sslrootcert=/run/servicedns.podcert.ate.dev/trust-bundle.pem&sslcert=/run/podidentity.podcert.ate.dev/credential-bundle.pem&sslkey=/run/podidentity.podcert.ate.dev/credential-bundle.pem&channel_binding=disable"
				tc.wantReadWrite = "postgresql://" + postgressetup.ReadWriteUser + ":" + postgressetup.ReadWritePassword + endpoint
				tc.wantOwner = "postgresql://" + postgressetup.OwnerUser + ":" + postgressetup.OwnerPassword + endpoint
				if postgres == nil || postgresConfig == nil {
					t.Fatal("missing bundled PostgreSQL setup")
				}
				if strings.TrimSpace(postgresConfig["setup.sql"]) != strings.TrimSpace(postgressetup.Script()) {
					t.Error("chart PostgreSQL setup differs from the shared installer script")
				}
				for _, rule := range []string{
					"hostssl all postgres all reject",
					"hostssl atepg all all scram-sha-256 clientcert=verify-ca",
				} {
					if !strings.Contains(postgresConfig["pg_hba.conf"], rule) {
						t.Errorf("missing PostgreSQL authentication rule %q", rule)
					}
				}
				bootstrapEnv := map[string]string{}
				for _, variable := range postgres.Env {
					bootstrapEnv[variable.Name] = variable.Value
				}
				for bootstrap, api := range map[string]string{
					"SUBSTRATE_SCHEMA":         "ATE_API_POSTGRES_SCHEMA",
					"SUBSTRATE_OWNER_ROLE":     "ATE_API_POSTGRES_OWNER_ROLE",
					"SUBSTRATE_READWRITE_ROLE": "ATE_API_POSTGRES_READ_WRITE_ROLE",
				} {
					if bootstrapEnv[bootstrap] == "" || bootstrapEnv[bootstrap] != env[api] {
						t.Errorf("bootstrap %s = %q, API %s = %q", bootstrap, bootstrapEnv[bootstrap], api, env[api])
					}
				}
			} else if postgres != nil || postgresConfig != nil {
				t.Error("external PostgreSQL configuration deploys a bundled database")
			}
			for key, want := range map[string]string{
				"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING": tc.wantReadWrite,
				"ATE_API_POSTGRES_OWNER_CONNECTION_STRING":      tc.wantOwner,
				"ATE_API_POSTGRES_READ_WRITE_ROLE":              tc.wantReadWriteRole,
				"ATE_API_POSTGRES_OWNER_ROLE":                   tc.wantOwnerRole,
			} {
				if env[key] != want {
					t.Errorf("%s = %q, want %q", key, env[key], want)
				}
				flag := "--" + strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(key, "ATE_API_")), "_", "-") + "=@env"
				found := false
				for _, arg := range container.Args {
					found = found || arg == flag
				}
				if !found {
					t.Errorf("missing %s", flag)
				}
			}
		})
	}
}
