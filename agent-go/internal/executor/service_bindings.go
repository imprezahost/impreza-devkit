package executor

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var bindingIDPattern = regexp.MustCompile(`^bnd_[a-f0-9]{24}$`)
var bindingDeploymentPattern = regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`)
var bindingRevisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var bindingVariablePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var bindingAdminPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

func validateServiceBindingRef(consumer string, r sdkclient.ServiceBindingRef) error {
	if !bindingDeploymentPattern.MatchString(consumer) || !bindingDeploymentPattern.MatchString(r.ProviderDeploymentID) || consumer == r.ProviderDeploymentID || !bindingIDPattern.MatchString(r.BindingID) || !bindingRevisionPattern.MatchString(r.Revision) || !bindingVariablePattern.MatchString(r.Variable) {
		return errors.New("invalid service binding identity")
	}
	if r.Variable != "DATABASE_URL" {
		return errors.New("unsupported service binding variable")
	}
	return nil
}

func validateServiceBinding(consumer string, credential sdkclient.ServiceBindingCredential) error {
	r := credential.ServiceBindingRef
	if err := validateServiceBindingRef(consumer, r); err != nil {
		return err
	}
	if credential.Username != "imp_"+strings.TrimPrefix(r.BindingID, "bnd_") || credential.Database != credential.Username || !bindingRevisionPattern.MatchString(credential.Password) || !bindingAdminPattern.MatchString(credential.AdminUser) {
		return errors.New("invalid service binding credential shape")
	}
	return nil
}

// Every interpolated value has a restrictive identifier/hex grammar. SQL never
// comes from the application, and errors/output must not expose this text.
func postgresBindingSQL(consumer string, c sdkclient.ServiceBindingCredential) (string, error) {
	if err := validateServiceBinding(consumer, c); err != nil {
		return "", err
	}
	verifier, err := postgresBindingVerifier(c.Password)
	if err != nil {
		return "", err
	}
	marker := "impreza:" + c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	role := c.Username
	sql := `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('` + marker + `',0));
BEGIN;
DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_roles WHERE rolname='` + role + `') THEN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='` + role + `' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND rolcanlogin AND NOT EXISTS (SELECT FROM pg_auth_members WHERE member=pg_roles.oid OR roleid=pg_roles.oid) AND shobj_description(oid,'pg_authid')='` + marker + `') THEN
   RAISE EXCEPTION 'Binding role ownership cannot be verified';
  END IF;
 ELSE
  CREATE ROLE "` + role + `" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '` + verifier + `';
  COMMENT ON ROLE "` + role + `" IS '` + marker + `';
 END IF;
END
$impreza$;
COMMIT;
SELECT format('CREATE DATABASE %I OWNER %I','` + c.Database + `','` + role + `') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='` + c.Database + `')
\gexec
DO $impreza$
BEGIN
 IF NOT EXISTS (SELECT FROM pg_database d JOIN pg_roles r ON r.oid=d.datdba WHERE d.datname='` + c.Database + `' AND r.rolname='` + role + `' AND shobj_description(r.oid,'pg_authid')='` + marker + `') THEN
  RAISE EXCEPTION 'Binding database ownership cannot be verified';
 END IF;
END
$impreza$;
REVOKE ALL ON DATABASE "` + c.Database + `" FROM PUBLIC;
`
	return sql, nil
}

// PostgreSQL accepts the SCRAM verifier directly. A failed SQL statement or
// administrator-enabled statement logging must never receive the plaintext
// password. Generated passwords are ASCII hex, so SASLprep is the identity.
func postgresBindingVerifier(password string) (string, error) {
	if !bindingRevisionPattern.MatchString(password) {
		return "", errors.New("invalid binding password")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("binding password verifier unavailable")
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, 4096, 32)
	if err != nil {
		return "", errors.New("binding password verifier unavailable")
	}
	client := hmac.New(sha256.New, key)
	_, _ = client.Write([]byte("Client Key"))
	stored := sha256.Sum256(client.Sum(nil))
	server := hmac.New(sha256.New, key)
	_, _ = server.Write([]byte("Server Key"))
	b64 := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$4096:" + b64(salt) + "$" + b64(stored[:]) + ":" + b64(server.Sum(nil)), nil
}

type bindingProviderContainer struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}
type bindingProviderNetwork struct {
	ID         string                     `json:"Id"`
	Name       string                     `json:"Name"`
	Driver     string                     `json:"Driver"`
	Labels     map[string]string          `json:"Labels"`
	Containers map[string]json.RawMessage `json:"Containers"`
}

func verifyBindingProvider(provider string, containers []bindingProviderContainer, networks []bindingProviderNetwork) error {
	if !bindingDeploymentPattern.MatchString(provider) || len(containers) != 1 || len(networks) != 1 {
		return errors.New("service provider identity unavailable")
	}
	c, n := containers[0], networks[0]
	if !bindingRevisionPattern.MatchString(c.ID) || !bindingRevisionPattern.MatchString(n.ID) || !c.State.Running || c.Config.Labels["com.docker.compose.project"] != provider || c.Config.Labels["com.docker.compose.service"] != "postgres" || n.Name != provider+"_default" || n.Driver != "bridge" || n.Labels["com.docker.compose.project"] != provider || n.Labels["com.docker.compose.network"] != "default" {
		return errors.New("service provider network ownership cannot be verified")
	}
	if _, ok := n.Containers[c.ID]; !ok {
		return errors.New("service provider is absent from its owned network")
	}
	return nil
}

func bindingProviderAddress(c bindingProviderContainer, n bindingProviderNetwork) (string, error) {
	var endpoint struct {
		IPv4Address string `json:"IPv4Address"`
	}
	if json.Unmarshal(n.Containers[c.ID], &endpoint) != nil {
		return "", errors.New("provider network address unavailable")
	}
	prefix, err := netip.ParsePrefix(endpoint.IPv4Address)
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() {
		return "", errors.New("provider needs a private IPv4 bridge address")
	}
	return prefix.Addr().String(), nil
}

// Never return command output: inspect can contain environment credentials and
// PostgreSQL errors can include statements. Only stable, generic errors escape.
func (d *Docker) provisionPostgresBinding(ctx context.Context, consumer string, c sdkclient.ServiceBindingCredential) (string, error) {
	sql, err := postgresBindingSQL(consumer, c)
	if err != nil {
		return "", err
	}
	return d.provisionPostgresBindingSQL(ctx, c, sql)
}

func (d *Docker) provisionPostgresBindingSQL(ctx context.Context, c sdkclient.ServiceBindingCredential, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var containers []bindingProviderContainer
	var networks []bindingProviderNetwork
	raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, "inspect", "pg_"+c.ProviderDeploymentID), 1024*1024)
	if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &containers) != nil {
		return "", errors.New("service provider container is unavailable")
	}
	raw, err = limitedRuntimeOutput(d.dockerCmd(ctx, "network", "inspect", c.ProviderDeploymentID+"_default"), 1024*1024)
	if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &networks) != nil {
		return "", errors.New("service provider network is unavailable")
	}
	if err = verifyBindingProvider(c.ProviderDeploymentID, containers, networks); err != nil {
		return "", err
	}
	address, err := bindingProviderAddress(containers[0], networks[0])
	if err != nil {
		return "", err
	}
	command := d.dockerCmd(ctx, "exec", "-i", "--user", "postgres", containers[0].ID, "psql", "--no-psqlrc", "--username", c.AdminUser, "--dbname", "postgres", "--quiet")
	command.Stdin = strings.NewReader(sql)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err = command.Run(); err != nil {
		return "", errors.New("service database provisioning failed; no credentials or SQL output were returned")
	}
	// Loopback can match pg_hba trust rules even when the bridge requires a
	// password. Use the inspected private address and an explicit negative
	// control: a successful query alone never proves password authentication.
	login := func(password string) error {
		verify := d.dockerCmd(ctx, "exec", "-i", "--user", "postgres", containers[0].ID, "sh", "-c", `IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql --no-psqlrc --host="$1" --username="$2" --dbname="$3" --no-password --quiet --set=ON_ERROR_STOP=1`, "binding-check", address, c.Username, c.Database)
		verify.Stdin = strings.NewReader(password + "\nSELECT 1;\n")
		verify.Stdout = io.Discard
		verify.Stderr = io.Discard
		return verify.Run()
	}
	wrong := "0" + c.Password[1:]
	if c.Password[0] == '0' {
		wrong = "1" + c.Password[1:]
	}
	if login(wrong) == nil {
		return "", errors.New("service provider accepted an invalid password; authentication cannot be verified")
	}
	if err = login(c.Password); err != nil {
		return "", errors.New("service database login could not be verified")
	}
	u := url.URL{Scheme: "postgresql", User: url.UserPassword(c.Username, c.Password), Host: "pg_" + c.ProviderDeploymentID + ":5432", Path: "/" + c.Database, RawQuery: "sslmode=disable"}
	return u.String(), nil
}

func validateServiceBindingManifest(p sdkclient.DeployPayload) error {
	runtime := p.Manifest.Runtime
	if runtime.ServiceBindingRotationProtocol != "" || runtime.ServiceBindingRotation != nil {
		return validateServiceBindingRotation(p)
	}
	if runtime.ServiceBindingRetirementProtocol != "" || len(runtime.ServiceBindingRetirements) != 0 {
		if (runtime.ServiceBindingRetirementProtocol != sdkclient.ServiceBindingRetirementProtocol && runtime.ServiceBindingRetirementProtocol != sdkclient.ServiceBindingGenerationRetirementProtocol && runtime.ServiceBindingRetirementProtocol != sdkclient.MysqlServiceBindingGenerationRetirementProtocol) || runtime.Type != "docker-compose" || runtime.Build != nil || len(runtime.ServiceBindingRetirements) != 1 || runtime.ServiceBindingProtocol != "" || len(runtime.ServiceBindings) != 0 {
			return errors.New("unsupported service binding retirement manifest")
		}
		ref := runtime.ServiceBindingRetirements[0]
		if err := validateServiceBindingRef(p.DeploymentID, ref); err != nil {
			return err
		}
		if _, exists := p.Vars[ref.Variable]; exists {
			return errors.New("retiring connection cannot contain a runtime credential")
		}
		return nil
	}
	if runtime.ServiceBindingProtocol == "" && len(runtime.ServiceBindings) == 0 {
		return nil
	}
	if (runtime.ServiceBindingProtocol != sdkclient.ServiceBindingProtocol && runtime.ServiceBindingProtocol != sdkclient.ServiceBindingGenerationProtocol && runtime.ServiceBindingProtocol != sdkclient.MysqlServiceBindingGenerationProtocol) || runtime.Type != "docker-compose" || len(runtime.ServiceBindings) != 1 || runtime.Build != nil {
		return errors.New("unsupported service binding manifest")
	}
	r := runtime.ServiceBindings[0]
	if err := validateServiceBindingRef(p.DeploymentID, r); err != nil {
		return err
	}
	if _, exists := p.Vars[r.Variable]; exists {
		return errors.New("service binding would overwrite a runtime variable")
	}
	return nil
}

// Fetch just in time; never place the provider's administrator password into
// the application payload. Mutate runtime variables only after provisioning.
func (d *Docker) prepareServiceBindings(ctx context.Context, cmd *sdkclient.PollCommand, p *sdkclient.DeployPayload) (map[string]string, error) {
	// A queued payload cannot authorize retirement. Only the JIT response can.
	p.ServiceBindingRetirementAuthorizations = nil
	if err := validateServiceBindingManifest(*p); err != nil {
		return nil, err
	}
	if len(p.Manifest.Runtime.ServiceBindings) == 0 {
		return nil, d.prepareServiceBindingRetirements(ctx, cmd, p)
	}
	if d.Client == nil || cmd.Kind != sdkclient.CommandDeploy || cmd.ControlToken == "" {
		return nil, errors.New("service bindings require an authenticated controlled deploy")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := d.Client.AgentServiceBindings(fetchCtx, p.DeploymentID, cmd.ID, cmd.ControlToken)
	if err != nil {
		return nil, errors.New("service credentials could not be obtained for this operation")
	}
	ref := p.Manifest.Runtime.ServiceBindings[0]
	if response == nil || response.Protocol != p.Manifest.Runtime.ServiceBindingProtocol || len(response.Bindings) != 1 || response.Bindings[0].ServiceBindingRef != ref {
		return nil, errors.New("service credential response does not match the reviewed binding")
	}
	credential := response.Bindings[0]
	if p.Manifest.Runtime.ServiceBindingRotation != nil {
		if err := d.prepareServiceBindingRotation(ctx, cmd, p); err != nil {
			return nil, err
		}
	}
	var value string
	switch response.Protocol {
	case sdkclient.ServiceBindingGenerationProtocol:
		value, err = d.provisionPostgresGeneration(ctx, p.DeploymentID, credential)
	case sdkclient.MysqlServiceBindingGenerationProtocol:
		value, err = d.provisionMysqlGeneration(ctx, p.DeploymentID, credential)
	default:
		value, err = d.provisionPostgresBinding(ctx, p.DeploymentID, credential)
	}
	if err != nil {
		return nil, err
	}
	vars := make(map[string]any, len(p.Vars)+1)
	for k, v := range p.Vars {
		vars[k] = v
	}
	vars[ref.Variable] = value
	p.Vars = vars
	return map[string]string{"url": value, "password": credential.Password}, nil
}

func redactServiceBindingResult(result *sdkclient.DeployResult, values map[string]string) {
	if len(values) == 0 {
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		result.Error = "Bound deployment failed; inspect private agent state."
		result.LogsTail = ""
		return
	}
	text := string(raw)
	for _, value := range values {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[redacted]")
		}
	}
	var clean sdkclient.DeployResult
	if json.Unmarshal([]byte(text), &clean) == nil {
		*result = clean
	} else {
		result.Error = "Bound deployment failed; inspect private agent state."
		result.LogsTail = ""
	}
}

// Route changes keep the already-authorized runtime credential locally. They
// never fetch a new credential or permit an API variable to replace it.
func preservedServiceBindingVars(consumer string, raw []byte, vars map[string]any) (map[string]any, error) {
	fail := func() (map[string]any, error) {
		return nil, errors.New("existing service binding could not be verified")
	}
	var model struct {
		Networks map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"networks"`
		Services map[string]struct {
			Networks    map[string]any `json:"networks"`
			Environment map[string]any `json:"environment"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &model) != nil {
		return fail()
	}
	binding, provider := "", ""
	for alias, network := range model.Networks {
		if !strings.HasPrefix(alias, "impreza-binding-") {
			continue
		}
		if binding != "" || !network.External {
			return fail()
		}
		binding = "bnd_" + strings.TrimPrefix(alias, "impreza-binding-")
		provider = strings.TrimSuffix(network.Name, "_default")
		if !strings.HasSuffix(network.Name, "_default") || !bindingIDPattern.MatchString(binding) || !bindingDeploymentPattern.MatchString(provider) || provider == consumer {
			return fail()
		}
	}
	if binding == "" {
		return vars, nil
	}
	if !bindingDeploymentPattern.MatchString(consumer) || len(model.Services) != 1 {
		return fail()
	}
	app, ok := model.Services["app"]
	if !ok {
		return fail()
	}
	if _, ok := app.Networks["impreza-binding-"+strings.TrimPrefix(binding, "bnd_")]; !ok {
		return fail()
	}
	if _, exists := vars["DATABASE_URL"]; exists {
		return nil, errors.New("service binding would overwrite a runtime variable")
	}
	value, ok := app.Environment["DATABASE_URL"].(string)
	if !ok {
		return fail()
	}
	u, err := url.Parse(value)
	if err != nil || u.User == nil {
		return fail()
	}
	password, hasPassword := u.User.Password()
	name := "imp_" + strings.TrimPrefix(binding, "bnd_")
	login := u.User.Username()
	generationLogin := strings.HasPrefix(login, "ibg_"+strings.TrimPrefix(binding, "bnd_")+"_") && bindingGenerationLoginPattern.MatchString(login)
	postgresURL := u.Scheme == "postgresql" && (login == name || generationLogin) && u.Host == "pg_"+provider+":5432" && u.Path == "/"+name && u.RawQuery == "sslmode=disable"
	mysqlURL := u.Scheme == "mysql" && generationLogin && u.Host == "mariadb_"+provider+":3306" && u.Path == "/"+name && u.RawQuery == ""
	if (!postgresURL && !mysqlURL) || !hasPassword || !bindingRevisionPattern.MatchString(password) || u.Fragment != "" || u.RawPath != "" {
		return fail()
	}
	copyVars := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		copyVars[k] = v
	}
	copyVars["DATABASE_URL"] = value
	return copyVars, nil
}

func (d *Docker) preserveServiceBindingVars(ctx context.Context, consumer string, vars map[string]any) (map[string]any, error) {
	if len(vars) == 0 {
		return vars, nil
	}
	path := filepath.Join(d.appDir(consumer), "compose.yaml")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("runtime configuration could not be verified")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("runtime configuration could not be read")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return nil, errors.New("runtime configuration could not be read")
	}
	if !strings.Contains(string(raw), "impreza-binding-") {
		return vars, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	command := d.dockerCmd(queryCtx, "compose", "config", "--format", "json")
	command.Dir = d.appDir(consumer)
	resolved, err := limitedRuntimeOutput(command, 1024*1024)
	if err != nil {
		return nil, errors.New("existing service binding could not be resolved")
	}
	return preservedServiceBindingVars(consumer, resolved, vars)
}

var bindingGenerationLoginPattern = regexp.MustCompile(`^ibg_[a-f0-9]{24}_[a-f0-9]{24}$`)
var bindingRuntimeURLPattern = regexp.MustCompile(`(?m)^DATABASE_URL=(postgresql://(?:imp_[a-f0-9]{24}|ibg_[a-f0-9]{24}_[a-f0-9]{24}):([a-f0-9]{64})@pg_dpl_(?:[a-f0-9]{16}|[a-f0-9]{24}):5432/imp_[a-f0-9]{24}\?sslmode=disable)\r?$`)
var mysqlBindingRuntimeURLPattern = regexp.MustCompile(`(?m)^DATABASE_URL=(mysql://ibg_[a-f0-9]{24}_[a-f0-9]{24}:([a-f0-9]{64})@mariadb_dpl_(?:[a-f0-9]{16}|[a-f0-9]{24}):3306/imp_[a-f0-9]{24})\r?$`)

var mysqlJobRuntimeURLPattern = regexp.MustCompile(`(?m)^(?:DATABASE_URL|IMPREZA_DATABASE_JOB_URL)=(mysql://ij[vr]_[a-f0-9]{16}:([a-f0-9]{64})@mariadb_dpl_(?:[a-f0-9]{16}|[a-f0-9]{24}):3306/imp_(?:verify|restore)_[a-f0-9]{16})\r?$`)

var postgresJobRuntimeURLPattern = regexp.MustCompile(`(?m)^(?:DATABASE_URL|IMPREZA_DATABASE_JOB_URL)=(postgresql://ij[vr]_[a-f0-9]{16}:([a-f0-9]{64})@pg_dpl_(?:[a-f0-9]{16}|[a-f0-9]{24}):5432/imp_(?:verify|restore)_[a-f0-9]{16}\?sslmode=disable)\r?$`)

func serviceBindingEnvRedactions(raw []byte) map[string]string {
	values := map[string]string{}
	for _, pattern := range []*regexp.Regexp{bindingRuntimeURLPattern, mysqlBindingRuntimeURLPattern, mysqlJobRuntimeURLPattern, postgresJobRuntimeURLPattern} {
		for _, match := range pattern.FindAllSubmatch(raw, -1) {
			values[string(match[1])] = string(match[1])
			values[string(match[2])] = string(match[2])
		}
	}
	return values
}

func redactServiceBindingText(text string, values map[string]string) string {
	for _, value := range values {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[redacted]")
		}
	}
	return text
}

func (d *Docker) runtimeServiceBindingRedactions(deployment string) (map[string]string, error) {
	if !bindingDeploymentPattern.MatchString(deployment) {
		return nil, nil
	}
	read := func(name string) ([]byte, error) {
		path := filepath.Join(d.appDir(deployment), name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
			return nil, errors.New("invalid private runtime file")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
		if len(raw) > 4*1024*1024 {
			return nil, errors.New("private runtime file exceeds limit")
		}
		return raw, err
	}
	compose, composeErr := read("compose.yaml")
	raw, envErr := read(".env")
	bound := strings.Contains(string(compose), "impreza-binding-")
	if bound && envErr != nil {
		return nil, errors.New("bound runtime credentials cannot be safely redacted")
	}
	if composeErr != nil && !os.IsNotExist(composeErr) {
		return nil, errors.New("runtime state cannot be safely inspected")
	}
	values := serviceBindingEnvRedactions(raw)
	if bound && len(values) == 0 {
		return nil, errors.New("bound runtime credentials cannot be safely redacted")
	}
	return values, nil
}
