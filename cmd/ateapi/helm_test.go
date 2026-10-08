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
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/postgressetup"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
				const endpoint = "test-postgres.custom.svc:5432/atepg"
				dsn := func(user string) string {
					bundle := "/run/postgres.podcert.ate.dev/" + user + ".pem"
					return fmt.Sprintf("postgresql://%s@%s?sslmode=verify-full&sslrootcert=/run/servicedns.podcert.ate.dev/trust-bundle.pem&sslcert=%s&sslkey=%s", user, endpoint, bundle, bundle)
				}
				tc.wantReadWrite = dsn(postgressetup.ReadWriteUser)
				tc.wantOwner = dsn(postgressetup.OwnerUser)
				if postgres == nil || postgresConfig == nil {
					t.Fatal("missing bundled PostgreSQL setup")
				}
				if strings.TrimSpace(postgresConfig["setup.sql"]) != strings.TrimSpace(postgressetup.Script()) {
					t.Error("chart PostgreSQL setup differs from the shared installer script")
				}
				for _, rule := range []string{
					"hostssl all postgres all reject",
					"hostssl atepg substrate_owner_user,substrate_readwrite_user all cert",
				} {
					if !strings.Contains(postgresConfig["pg_hba.conf"], rule) {
						t.Errorf("missing PostgreSQL authentication rule %q", rule)
					}
				}
				if !strings.Contains(postgresConfig["postgresql.conf"], "ssl_ca_file = '/run/postgres.podcert.ate.dev/trust-bundle.pem'") ||
					!strings.Contains(postgresConfig["reload-tls.sh"], "CA=/run/postgres.podcert.ate.dev/trust-bundle.pem") {
					t.Error("PostgreSQL and its TLS reloader must use the dedicated client CA")
				}
				bootstrapCommand := strings.Join(postgres.Lifecycle.PostStart.Exec.Command, " ")
				for _, user := range []string{"owner", "readwrite"} {
					if !strings.Contains(bootstrapCommand, "--set=substrate_"+user+"_password=\"\"") {
						t.Errorf("bootstrap must clear the %s login password", user)
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

func TestHelmPostgresCertificates(t *testing.T) {
	for _, tc := range []struct {
		release, namespace string
		external           bool
	}{
		{release: "substrate", namespace: "ate-system"},
		{release: "team", namespace: "custom"},
		{release: "team", namespace: "custom", external: true},
	} {
		t.Run(fmt.Sprintf("%s/%s/external=%t", tc.release, tc.namespace, tc.external), func(t *testing.T) {
			args := []string{"template", tc.release, "../../charts/substrate", "-n", tc.namespace}
			if tc.external {
				args = append(args, "--set", "postgres.enabled=false", "--set", "postgres.readWriteConnectionString=postgresql://login@db/atepg")
			}
			out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			var api, controller corev1.PodSpec
			var signerPermission bool
			for _, doc := range strings.Split(string(out), "\n---\n") {
				var deployment appsv1.Deployment
				if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
					t.Fatal(err)
				}
				if deployment.Kind == "Deployment" {
					switch deployment.Spec.Template.Labels["app"] {
					case "ate-api-server":
						api = deployment.Spec.Template.Spec
					case "podcertificate-controller":
						controller = deployment.Spec.Template.Spec
					}
				}
				if deployment.Kind == "ClusterRole" {
					var role rbacv1.ClusterRole
					if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
						t.Fatal(err)
					}
					for _, rule := range role.Rules {
						if slices.Contains(rule.Resources, "signers") && slices.Contains(rule.ResourceNames, "postgres.podcert.ate.dev/*") && slices.Contains(rule.Verbs, "sign") && slices.Contains(rule.Verbs, "attest") {
							signerPermission = true
						}
					}
				}
			}
			if api.ServiceAccountName == "" || len(controller.Containers) != 1 || !signerPermission {
				t.Fatal("missing API identity, certificate controller, or PostgreSQL signer permission")
			}
			for _, arg := range []string{
				"--postgres-client-namespace=" + tc.namespace,
				"--postgres-client-service-account=" + api.ServiceAccountName,
				"--postgres-ca-pool=/run/ca-state/postgres-pool.json",
			} {
				if !slices.Contains(controller.Containers[0].Args, arg) {
					t.Errorf("controller is missing %s", arg)
				}
			}
			var caSecret bool
			for _, volume := range controller.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil && source.Secret.Name == "postgres-ca-pool" {
						caSecret = slices.Contains(source.Secret.Items, corev1.KeyToPath{Key: "pool", Path: "postgres-pool.json"})
					}
				}
			}
			if !caSecret {
				t.Error("controller is missing its PostgreSQL CA pool")
			}
			bundles := map[string]string{}
			for _, volume := range api.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					cert := source.PodCertificate
					if cert != nil && cert.SignerName == "postgres.podcert.ate.dev/identity" {
						bundles[cert.CredentialBundlePath] = cert.UserAnnotations["postgres.podcert.ate.dev/username"]
					}
				}
			}
			if tc.external {
				if len(bundles) != 0 {
					t.Error("external database install requests bundled login certificates")
				}
				return
			}
			if len(bundles) != 2 {
				t.Fatalf("got %d PostgreSQL certificate projections, want two", len(bundles))
			}
			for _, user := range []string{postgressetup.OwnerUser, postgressetup.ReadWriteUser} {
				if bundles[user+".pem"] != user {
					t.Errorf("missing separate certificate for %s", user)
				}
			}
			var mounted bool
			for _, container := range api.Containers {
				if container.Name == "ate-api-server" {
					for _, mount := range container.VolumeMounts {
						mounted = mounted || (mount.Name == "postgres" && mount.MountPath == "/run/postgres.podcert.ate.dev" && mount.ReadOnly)
					}
				}
			}
			if !mounted {
				t.Error("API server cannot read its projected PostgreSQL certificates")
			}
		})
	}
}
