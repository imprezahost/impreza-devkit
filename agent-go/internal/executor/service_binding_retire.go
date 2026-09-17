package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func (d *Docker) prepareServiceBindingRetirements(ctx context.Context, cmd *sdkclient.PollCommand, p *sdkclient.DeployPayload) error {
	refs := p.Manifest.Runtime.ServiceBindingRetirements
	if len(refs) == 0 {
		return nil
	}
	if d.Client == nil || cmd.Kind != sdkclient.CommandDeploy || cmd.ControlToken == "" {
		return errors.New("connection retirement requires an authenticated controlled deploy")
	}
	fetch, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := d.Client.AgentServiceBindingRetirements(fetch, p.DeploymentID, cmd.ID, cmd.ControlToken)
	if err != nil || response == nil || response.Protocol != p.Manifest.Runtime.ServiceBindingRetirementProtocol || len(response.Retirements) != 1 || response.Retirements[0].ServiceBindingRef != refs[0] {
		return errors.New("connection retirement authorization does not match the reviewed operation")
	}
	candidate := *p
	candidate.ServiceBindingRetirementAuthorizations = response.Retirements
	if err := validateResolvedRetirements(candidate); err != nil {
		return err
	}
	p.ServiceBindingRetirementAuthorizations = response.Retirements
	return nil
}

func validateResolvedRetirements(p sdkclient.DeployPayload) error {
	refs := p.Manifest.Runtime.ServiceBindingRetirements
	if p.Manifest.Runtime.ServiceBindingRotation != nil || p.Manifest.Runtime.ServiceBindingRotationProtocol != "" {
		return validateResolvedRotation(p)
	}
	if len(refs) == 0 && len(p.ServiceBindingRetirementAuthorizations) == 0 && p.Manifest.Runtime.ServiceBindingRetirementProtocol == "" {
		return nil
	}
	if err := validateServiceBindingManifest(p); err != nil {
		return err
	}
	if len(refs) != 1 || len(p.ServiceBindingRetirementAuthorizations) != 1 || refs[0] != p.ServiceBindingRetirementAuthorizations[0].ServiceBindingRef {
		return errors.New("verified connection retirement authorization is missing")
	}
	if p.Manifest.Runtime.ServiceBindingRetirementProtocol == sdkclient.ServiceBindingGenerationRetirementProtocol {
		_, err := postgresGenerationRetireSQL(p.DeploymentID, p.ServiceBindingRetirementAuthorizations[0])
		return err
	}
	_, err := postgresBindingRetireSQL(p.DeploymentID, p.ServiceBindingRetirementAuthorizations[0])
	return err
}

// Verify the actual serving container, not just the intended Compose network.
func (d *Docker) verifyConsumerDisconnected(ctx context.Context, consumer, provider string) error {
	ids, err := d.recoveryContainers(ctx, consumer)
	if err != nil || len(ids) != 1 {
		return errors.New("consumer replacement identity cannot be verified")
	}
	raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, append([]string{"inspect"}, ids...)...), 1024*1024)
	var containers []struct {
		ID     string
		Config struct {
			Labels map[string]string
			Env    []string
		}
		State           struct{ Running bool }
		NetworkSettings struct{ Networks map[string]json.RawMessage }
	}
	if err != nil || json.Unmarshal(raw, &containers) != nil || len(containers) != 1 {
		return errors.New("consumer replacement cannot be inspected")
	}
	c := containers[0]
	if c.ID != ids[0] || !c.State.Running || c.Config.Labels["com.docker.compose.project"] != consumer || c.Config.Labels["com.docker.compose.service"] != "app" {
		return errors.New("consumer replacement ownership cannot be verified")
	}
	if _, exists := c.NetworkSettings.Networks[provider+"_default"]; exists {
		return errors.New("consumer is still attached to the database network")
	}
	for _, value := range c.Config.Env {
		if strings.HasPrefix(value, "DATABASE_URL=") {
			return errors.New("consumer still has a database connection variable")
		}
	}
	return nil
}

func (d *Docker) finishServiceBindingRetirements(ctx context.Context, p sdkclient.DeployPayload) []sdkclient.ServiceBindingRetirementResult {
	// A rotation carries its target in the same authorization slot; its outcome
	// is reported through the rotation field, never as a removal result.
	if p.Manifest.Runtime.ServiceBindingRotation != nil {
		return nil
	}
	var results []sdkclient.ServiceBindingRetirementResult
	verified := validateResolvedRetirements(p) == nil
	for _, c := range p.ServiceBindingRetirementAuthorizations {
		out := sdkclient.ServiceBindingRetirementResult{ServiceBindingRef: c.ServiceBindingRef, Status: "pending", DataRetained: true}
		check, cancel := context.WithTimeout(ctx, 60*time.Second)
		if verified && d.verifyConsumerDisconnected(check, p.DeploymentID, c.ProviderDeploymentID) == nil {
			var err error
			if p.Manifest.Runtime.ServiceBindingRetirementProtocol == sdkclient.ServiceBindingGenerationRetirementProtocol {
				err = d.retirePostgresGeneration(check, p.DeploymentID, c)
			} else {
				err = d.retirePostgresBinding(check, p.DeploymentID, c)
			}
			if err == nil {
				out.Status = "retired"
			}
		}
		cancel()
		results = append(results, out)
	}
	return results
}

// This primitive is not dispatched until the API has proved a successful
// consumer replacement. It cannot delete a database or adopt an unmarked role.
func postgresBindingRetireSQL(consumer string, c sdkclient.ServiceBindingRetirement) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	if c.Username != "imp_"+strings.TrimPrefix(c.BindingID, "bnd_") || c.Database != c.Username || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid binding retirement identity")
	}
	marker := "impreza:" + c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	role := c.Username
	return `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('` + marker + `',0));
BEGIN;
DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_roles WHERE rolname='` + role + `') THEN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='` + role + `' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT EXISTS (SELECT FROM pg_auth_members WHERE member=pg_roles.oid OR roleid=pg_roles.oid) AND shobj_description(oid,'pg_authid')='` + marker + `') THEN
   RAISE EXCEPTION 'Binding retirement role ownership cannot be verified';
  END IF;
  IF EXISTS (SELECT FROM pg_database WHERE datname='` + c.Database + `') AND NOT EXISTS (SELECT FROM pg_database d JOIN pg_roles r ON r.oid=d.datdba WHERE d.datname='` + c.Database + `' AND r.rolname='` + role + `') THEN
   RAISE EXCEPTION 'Binding retirement database ownership cannot be verified';
  END IF;
 ELSIF EXISTS (SELECT FROM pg_database WHERE datname='` + c.Database + `') THEN
  RAISE EXCEPTION 'Binding retirement role is missing for existing data';
 END IF;
END
$impreza$;
SELECT format('ALTER ROLE %I NOLOGIN PASSWORD NULL','` + role + `') WHERE EXISTS (SELECT FROM pg_roles WHERE rolname='` + role + `')
\gexec
COMMIT;
SELECT pg_terminate_backend(pid,5000) FROM pg_stat_activity WHERE usename='` + role + `' AND pid<>pg_backend_pid();
DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_authid WHERE rolname='` + role + `' AND (rolcanlogin OR rolpassword IS NOT NULL)) OR EXISTS (SELECT FROM pg_stat_activity WHERE usename='` + role + `') THEN
  RAISE EXCEPTION 'Binding retirement is not complete';
 END IF;
END
$impreza$;
`, nil
}

func (d *Docker) retirePostgresBinding(ctx context.Context, consumer string, c sdkclient.ServiceBindingRetirement) error {
	sql, err := postgresBindingRetireSQL(consumer, c)
	if err != nil {
		return err
	}
	return d.retirePostgresBindingSQL(ctx, c, "postgres", sql)
}

func (d *Docker) retirePostgresBindingSQL(ctx context.Context, c sdkclient.ServiceBindingRetirement, database, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var containers []bindingProviderContainer
	var networks []bindingProviderNetwork
	raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, "inspect", "pg_"+c.ProviderDeploymentID), 1024*1024)
	if err != nil || json.Unmarshal(raw, &containers) != nil {
		return errors.New("service provider container unavailable for retirement")
	}
	raw, err = limitedRuntimeOutput(d.dockerCmd(ctx, "network", "inspect", c.ProviderDeploymentID+"_default"), 1024*1024)
	if err != nil || json.Unmarshal(raw, &networks) != nil {
		return errors.New("service provider network unavailable for retirement")
	}
	if err = verifyBindingProvider(c.ProviderDeploymentID, containers, networks); err != nil {
		return err
	}
	if _, err = bindingProviderAddress(containers[0], networks[0]); err != nil {
		return err
	}
	cmd := d.dockerCmd(ctx, "exec", "-i", "--user", "postgres", containers[0].ID, "psql", "--no-psqlrc", "--username", c.AdminUser, "--dbname", database, "--quiet")
	cmd.Stdin = strings.NewReader(sql)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return errors.New("service login retirement could not be verified; database data was not deleted")
	}
	return nil
}
