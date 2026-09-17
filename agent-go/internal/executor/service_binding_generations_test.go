package executor

import (
	"strings"
	"testing"

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
