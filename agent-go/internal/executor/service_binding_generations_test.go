package executor

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func generationFixture() (string, sdkclient.ServiceBindingCredential) {
	consumer, c := bindingFixture()
	c.BindingID = "bnd_9876543210abcdef98765432"
	c.Revision = strings.Repeat("1", 64)
	c.Password = strings.Repeat("e", 64)
	c.Database, _, c.Username = bindingGenerationNames(c.ServiceBindingRef)
	return consumer, c
}

func TestServiceBindingGenerationInput(t *testing.T) {
	consumer, c := generationFixture()
	sql, err := postgresGenerationSQL(consumer, c)
	if err != nil || strings.Contains(sql, c.Password) || !strings.Contains(sql, "SCRAM-SHA-256$") {
		t.Fatal("generation SQL did not preserve credential boundary")
	}
	for name, change := range map[string]func(*sdkclient.ServiceBindingCredential){
		"login":    func(v *sdkclient.ServiceBindingCredential) { v.Username = "postgres" },
		"database": func(v *sdkclient.ServiceBindingCredential) { v.Database = "postgres" },
		"revision": func(v *sdkclient.ServiceBindingCredential) { v.Revision = "short" },
		"password": func(v *sdkclient.ServiceBindingCredential) { v.Password = "bad'password" },
		"admin":    func(v *sdkclient.ServiceBindingCredential) { v.AdminUser = "--help" },
		"provider": func(v *sdkclient.ServiceBindingCredential) { v.ProviderDeploymentID = consumer },
	} {
		t.Run(name, func(t *testing.T) {
			v := c
			change(&v)
			if sql, e := postgresGenerationSQL(consumer, v); e == nil || sql != "" {
				t.Fatal("invalid generation reached SQL")
			}
		})
	}
}

func TestServiceBindingGenerationsLive(t *testing.T) {
	if os.Getenv("IMPREZA_BINDING_TEST_RUN") == "" {
		t.Skip("isolated lease required")
	}
	var lease struct {
		Run     string `json:"run_id"`
		Account int    `json:"account_id"`
		Service int    `json:"service_id"`
		Status  string `json:"status"`
	}
	raw, err := os.ReadFile("/var/lib/impreza-test-vps/lease.json")
	if err != nil || json.Unmarshal(raw, &lease) != nil || os.Getenv("IMPREZA_BINDING_TEST_RUN") != lease.Run || lease.Account != 1 || lease.Service <= 0 || lease.Status != "Active" {
		t.Fatal("generation test lease mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	d := &Docker{StateDir: t.TempDir()}
	consumer, first := generationFixture()
	_, owner, _ := bindingGenerationNames(first.ServiceBindingRef)
	admin := func(database, query string) string {
		t.Helper()
		cmd := d.dockerCmd(ctx, "exec", "-i", "--user", "postgres", "pg_"+first.ProviderDeploymentID, "psql", "--no-psqlrc", "-U", "postgres", "-d", database, "-At", "-v", "ON_ERROR_STOP=1")
		cmd.Stdin = strings.NewReader(query)
		cmd.Stderr = io.Discard
		out, e := cmd.Output()
		if e != nil {
			t.Fatal("generation administrative fixture failed")
		}
		return strings.TrimSpace(string(out))
	}
	login := func(c sdkclient.ServiceBindingCredential, database, query string) (string, error) {
		cmd := d.dockerCmd(ctx, "run", "--rm", "-i", "--network", c.ProviderDeploymentID+"_default", "postgres:17", "sh", "-c", `IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql --no-psqlrc --host="$1" --username="$2" --dbname="$3" --no-password -At --set=ON_ERROR_STOP=1`, "generation-client", "pg_"+c.ProviderDeploymentID, c.Username, database)
		cmd.Stdin = strings.NewReader(c.Password + "\n" + query + "\n")
		cmd.Stderr = io.Discard
		out, e := cmd.Output()
		return strings.TrimSpace(string(out)), e
	}
	second := first
	second.Revision = strings.Repeat("2", 64)
	second.Password = strings.Repeat("f", 64)
	second.Database, _, second.Username = bindingGenerationNames(second.ServiceBindingRef)
	t.Run("stable_owner_and_distinct_logins", func(t *testing.T) {
		if _, e := d.provisionPostgresGeneration(ctx, consumer, first); e != nil {
			t.Fatal(e)
		}
		if got, e := login(first, first.Database, "SELECT current_user,session_user;"); e != nil || got != owner+"|"+first.Username {
			t.Fatal("generation session does not use stable owner")
		}
		if _, e := login(first, first.Database, "CREATE TABLE generation_data(n integer); INSERT INTO generation_data VALUES(73);"); e != nil {
			t.Fatal("generation cannot create its data")
		}
		if got := admin(first.Database, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE relname='generation_data';"); got != owner {
			t.Fatal("new table is not owned by stable role")
		}
		if _, e := d.provisionPostgresGeneration(ctx, consumer, second); e != nil {
			t.Fatal(e)
		}
		for _, c := range []sdkclient.ServiceBindingCredential{first, second} {
			if got, e := login(c, c.Database, "SELECT n FROM generation_data;"); e != nil || got != "73" {
				t.Fatal("new generation broke serving login or data")
			}
		}
		if got := admin("postgres", "SELECT rolcanlogin,rolpassword IS NULL FROM pg_authid WHERE rolname='"+owner+"';"); got != "f|t" {
			t.Fatal("stable owner has usable login")
		}
	})
	if t.Failed() {
		return
	}
	t.Run("no_password_reset_or_revision_collision", func(t *testing.T) {
		wrong := second
		wrong.Password = strings.Repeat("0", 64)
		if _, e := d.provisionPostgresGeneration(ctx, consumer, wrong); e == nil {
			t.Fatal("wrong existing password adopted")
		}
		collision := second
		collision.Revision = second.Revision[:24] + strings.Repeat("3", 40)
		if _, e := d.provisionPostgresGeneration(ctx, consumer, collision); e == nil {
			t.Fatal("truncated revision collision adopted")
		}
		if got, e := login(second, second.Database, "SELECT n FROM generation_data;"); e != nil || got != "73" {
			t.Fatal("refused candidate changed serving login")
		}
	})
	t.Run("no_role_or_cross_binding_privilege", func(t *testing.T) {
		for _, query := range []string{"CREATE ROLE forbidden;", "CREATE DATABASE forbidden;", "SET ROLE postgres;"} {
			if _, e := login(first, first.Database, query); e == nil {
				t.Fatal("generation escaped application authority")
			}
		}
		other := first
		other.BindingID = "bnd_abcdef9876543210abcdef98"
		other.Database, _, other.Username = bindingGenerationNames(other.ServiceBindingRef)
		if _, e := d.provisionPostgresGeneration(ctx, consumer, other); e != nil {
			t.Fatal(e)
		}
		if _, e := login(first, other.Database, "SELECT 1;"); e == nil {
			t.Fatal("generation connected to another binding")
		}
	})
	t.Run("membership_owner_and_configuration_drift", func(t *testing.T) {
		admin("postgres", "CREATE ROLE generation_outsider NOLOGIN; GRANT \""+owner+"\" TO generation_outsider;")
		if _, e := d.provisionPostgresGeneration(ctx, consumer, second); e == nil {
			t.Fatal("foreign owner member accepted")
		}
		admin("postgres", "REVOKE \""+owner+"\" FROM generation_outsider; ALTER ROLE \""+owner+"\" CREATEDB;")
		if _, e := d.provisionPostgresGeneration(ctx, consumer, second); e == nil {
			t.Fatal("elevated owner accepted")
		}
		admin("postgres", "ALTER ROLE \""+owner+"\" NOCREATEDB; ALTER ROLE \""+second.Username+"\" IN DATABASE \""+second.Database+"\" SET role TO 'postgres';")
		if _, e := d.provisionPostgresGeneration(ctx, consumer, second); e == nil {
			t.Fatal("foreign session role overwritten")
		}
		admin("postgres", "ALTER ROLE \""+second.Username+"\" IN DATABASE \""+second.Database+"\" SET role TO '"+owner+"';")
		if _, e := d.provisionPostgresGeneration(ctx, consumer, second); e != nil {
			t.Fatal("verified generation could not be reused")
		}
	})
	t.Run("legacy_owner_is_not_migrated_implicitly", func(t *testing.T) {
		legacy := first
		legacy.BindingID = "bnd_76543210abcdef9876543210"
		legacy.Username = "imp_" + strings.TrimPrefix(legacy.BindingID, "bnd_")
		legacy.Database = legacy.Username
		if _, e := d.provisionPostgresBinding(ctx, consumer, legacy); e != nil {
			t.Fatal(e)
		}
		candidate := legacy
		candidate.Database, _, candidate.Username = bindingGenerationNames(candidate.ServiceBindingRef)
		if _, e := d.provisionPostgresGeneration(ctx, consumer, candidate); e == nil {
			t.Fatal("legacy binding silently migrated")
		}
		if _, e := login(legacy, legacy.Database, "SELECT 1;"); e != nil {
			t.Fatal("legacy login changed by refused adoption")
		}
	})
	t.Run("foreign_login_refused_before_database_creation", func(t *testing.T) {
		foreign := first
		foreign.BindingID = "bnd_0123456789abcdef01234567"
		var foreignOwner string
		foreign.Database, foreignOwner, foreign.Username = bindingGenerationNames(foreign.ServiceBindingRef)
		admin("postgres", "CREATE ROLE \""+foreign.Username+"\" NOLOGIN;")
		if _, e := d.provisionPostgresGeneration(ctx, consumer, foreign); e == nil {
			t.Fatal("foreign generation role adopted")
		}
		if admin("postgres", "SELECT count(*) FROM pg_roles WHERE rolname='"+foreignOwner+"';") != "0" || admin("postgres", "SELECT count(*) FROM pg_database WHERE datname='"+foreign.Database+"';") != "0" {
			t.Fatal("foreign login refusal created owner or database")
		}
	})
	t.Run("retirement_preserves_data_and_other_generation", func(t *testing.T) {
		retirement := sdkclient.ServiceBindingRetirement{ServiceBindingRef: first.ServiceBindingRef, Username: first.Username, Database: first.Database, AdminUser: first.AdminUser}
		admin("postgres", "GRANT \""+owner+"\" TO generation_outsider;")
		if e := d.retirePostgresGeneration(ctx, consumer, retirement); e == nil {
			t.Fatal("retirement accepted an unmarked owner member")
		}
		admin("postgres", "REVOKE \""+owner+"\" FROM generation_outsider;")
		if _, e := login(first, first.Database, "SELECT 1;"); e != nil {
			t.Fatal("membership refusal disabled serving login")
		}
		wrong := retirement
		wrong.Revision = first.Revision[:24] + strings.Repeat("4", 40)
		if e := d.retirePostgresGeneration(ctx, consumer, wrong); e == nil {
			t.Fatal("retirement accepted a truncated revision collision")
		}
		admin("postgres", "ALTER ROLE \""+first.Username+"\" CREATEDB;")
		if e := d.retirePostgresGeneration(ctx, consumer, retirement); e == nil {
			t.Fatal("retirement accepted elevated login")
		}
		admin("postgres", "ALTER ROLE \""+first.Username+"\" NOCREATEDB;")
		foreignDB := "imp_abcdef9876543210abcdef98"
		admin(foreignDB, "CREATE TABLE foreign_owned(n integer); ALTER TABLE foreign_owned OWNER TO \""+first.Username+"\";")
		if e := d.retirePostgresGeneration(ctx, consumer, retirement); e == nil {
			t.Fatal("retirement accepted ownership inside another database")
		}
		admin(foreignDB, "ALTER TABLE foreign_owned OWNER TO ibo_abcdef9876543210abcdef98;")
		// A client can explicitly reset its role; these objects must also survive.
		if _, e := login(first, first.Database, "SET ROLE NONE; CREATE TABLE login_owned(n integer); INSERT INTO login_owned VALUES(91);"); e != nil {
			t.Fatal("could not create login-owned fixture data")
		}
		admin("postgres", "CREATE DATABASE generation_foreign OWNER \""+first.Username+"\";")
		if e := d.retirePostgresGeneration(ctx, consumer, retirement); e == nil {
			t.Fatal("retirement accepted foreign shared ownership")
		}
		if _, e := login(first, first.Database, "SELECT 1;"); e != nil {
			t.Fatal("foreign ownership refusal disabled serving login")
		}
		admin("postgres", "DROP DATABASE generation_foreign;")
		if _, e := login(first, first.Database, "BEGIN; INSERT INTO generation_data VALUES(74); PREPARE TRANSACTION 'generation-retire-test';"); e != nil {
			t.Fatal("could not prepare customer transaction fixture")
		}
		if e := d.retirePostgresGeneration(ctx, consumer, retirement); e == nil {
			t.Fatal("retirement accepted unresolved prepared transaction")
		}
		if got := admin(first.Database, "SELECT count(*) FROM pg_prepared_xacts WHERE gid='generation-retire-test';"); got != "1" {
			t.Fatal("retirement altered a customer prepared transaction")
		}
		admin(first.Database, "ROLLBACK PREPARED 'generation-retire-test';")
		if _, e := login(first, first.Database, "SELECT 1;"); e != nil {
			t.Fatal("prepared transaction refusal disabled serving login")
		}
		activeName := "generation-retire-session"
		active := d.dockerCmd(ctx, "run", "--rm", "--name", activeName, "-i", "--network", first.ProviderDeploymentID+"_default", "postgres:17", "sh", "-c", `IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql --no-psqlrc --host="$1" --username="$2" --dbname="$3" --no-password -At --set=ON_ERROR_STOP=1`, "generation-client", "pg_"+first.ProviderDeploymentID, first.Username, first.Database)
		active.Stdin = strings.NewReader(first.Password + "\nSELECT pg_sleep(60);\n")
		active.Stdout, active.Stderr = io.Discard, io.Discard
		if e := active.Start(); e != nil {
			t.Fatal("could not start active generation session")
		}
		finished := make(chan error, 1)
		go func() { finished <- active.Wait() }()
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			_ = d.dockerCmd(cleanup, "rm", "-f", activeName).Run()
		}()
		ready := false
		for i := 0; i < 40; i++ {
			if admin("postgres", "SELECT count(*) FROM pg_stat_activity WHERE usename='"+first.Username+"' AND state='active';") == "1" {
				ready = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ready {
			t.Fatal("active generation session did not reach PostgreSQL")
		}
		for i := 0; i < 2; i++ {
			if e := d.retirePostgresGeneration(ctx, consumer, retirement); e != nil {
				t.Fatal("generation retirement/retry failed", e)
			}
		}
		select {
		case e := <-finished:
			if e == nil {
				t.Fatal("active session completed instead of being terminated")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("retired generation kept an active session")
		}
		if _, e := login(first, first.Database, "SELECT 1;"); e == nil {
			t.Fatal("retired login still authenticates")
		}
		if got, e := login(second, second.Database, "SELECT n FROM generation_data; SELECT n FROM login_owned;"); e != nil || got != "73\n91" {
			t.Fatal("retirement lost data or broke replacement generation")
		}
		if admin(first.Database, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE relname='login_owned';") != owner {
			t.Fatal("login-owned table was not retained by stable owner")
		}
		if admin("postgres", "SELECT rolcanlogin,rolpassword IS NULL,(SELECT count(*) FROM pg_auth_members WHERE member=r.oid) FROM pg_authid r WHERE rolname='"+first.Username+"';") != "f|t|0" {
			t.Fatal("retired generation still has login, password or membership")
		}
		if _, e := d.provisionPostgresGeneration(ctx, consumer, first); e == nil {
			t.Fatal("retired generation was reactivated")
		}
	})
}

func TestServiceBindingGenerationRetireInput(t *testing.T) {
	consumer, c := generationFixture()
	r := sdkclient.ServiceBindingRetirement{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}
	if sql, err := postgresGenerationRetireSQL(consumer, r); err != nil || sql == "" {
		t.Fatal("valid generation retirement refused")
	}
	for name, change := range map[string]func(*sdkclient.ServiceBindingRetirement){
		"login":    func(v *sdkclient.ServiceBindingRetirement) { v.Username = "postgres" },
		"database": func(v *sdkclient.ServiceBindingRetirement) { v.Database = "postgres" },
		"revision": func(v *sdkclient.ServiceBindingRetirement) { v.Revision = "'" },
		"admin":    func(v *sdkclient.ServiceBindingRetirement) { v.AdminUser = "--help" },
		"provider": func(v *sdkclient.ServiceBindingRetirement) { v.ProviderDeploymentID = consumer },
	} {
		t.Run(name, func(t *testing.T) {
			v := r
			change(&v)
			if sql, err := postgresGenerationRetireSQL(consumer, v); err == nil || sql != "" {
				t.Fatal("untrusted generation retirement reached SQL")
			}
		})
	}
}

// A reviewed restore deliberately creates imp_restore_* databases owned by
// the binding's stable owner; the owner invariant must exempt exactly the
// databases carrying this binding's restore marker, or the first deploy
// after a restore fails with "Generation owner role cannot be verified"
// (measured by the cross-host mobility matrix on 2026-09-21).
func TestServiceBindingGenerationExemptsRestoreDatabases(t *testing.T) {
	consumer, c := generationFixture()
	sql, err := postgresGenerationSQL(consumer, c)
	if err != nil {
		t.Fatal(err)
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	exemption := "IS DISTINCT FROM 'impreza-restore:" + identity + "'"
	if !strings.Contains(sql, exemption) {
		t.Fatalf("provision SQL does not exempt this binding's restore databases:\n%s", sql)
	}
	if !strings.Contains(sql, "shobj_description(oid,'pg_database')") {
		t.Fatal("exemption must key on the catalog marker, not the database name")
	}
	retirement := sdkclient.ServiceBindingRetirement{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}
	retireSQL, err := postgresGenerationRetireSQL(consumer, retirement)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retireSQL, exemption) {
		t.Fatalf("retire SQL does not exempt this binding's restore databases:\n%s", retireSQL)
	}
}
