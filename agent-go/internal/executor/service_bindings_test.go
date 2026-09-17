package executor

import (
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"strings"
	"testing"
)

func TestServiceBindingProviderAddress(t *testing.T) {
	c := bindingProviderContainer{ID: strings.Repeat("1", 64)}
	for _, address := range []string{"127.0.0.1/8", "8.8.8.8/24", "::1/128", "169.254.169.254/32", "172.18.0.2;bad", "172.18.0.2"} {
		n := bindingProviderNetwork{Containers: map[string]json.RawMessage{c.ID: json.RawMessage(`{"IPv4Address":"` + address + `"}`)}}
		if _, err := bindingProviderAddress(c, n); err == nil {
			t.Fatal("invalid provider address accepted")
		}
	}
	n := bindingProviderNetwork{Containers: map[string]json.RawMessage{c.ID: json.RawMessage(`{"IPv4Address":"172.18.0.2/16"}`)}}
	if address, err := bindingProviderAddress(c, n); err != nil || address != "172.18.0.2" {
		t.Fatal("verified bridge address rejected")
	}
}

func bindingFixture() (string, sdkclient.ServiceBindingCredential) {
	return "dpl_" + strings.Repeat("c", 16), sdkclient.ServiceBindingCredential{
		ServiceBindingRef: sdkclient.ServiceBindingRef{BindingID: "bnd_" + strings.Repeat("a", 24), ProviderDeploymentID: "dpl_" + strings.Repeat("b", 16), Variable: "DATABASE_URL", Revision: strings.Repeat("d", 64)},
		Username:          "imp_" + strings.Repeat("a", 24), Database: "imp_" + strings.Repeat("a", 24), Password: strings.Repeat("e", 64), AdminUser: "postgres",
	}
}

func TestServiceBindingUntrustedFields(t *testing.T) {
	consumer, base := bindingFixture()
	if sql, err := postgresBindingSQL(consumer, base); err != nil || strings.Contains(sql, base.Password) || !strings.Contains(sql, "SCRAM-SHA-256$4096:") {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*sdkclient.ServiceBindingCredential){
		"binding SQL":       func(c *sdkclient.ServiceBindingCredential) { c.BindingID = "bnd_'; DROP ROLE postgres; --" },
		"provider path":     func(c *sdkclient.ServiceBindingCredential) { c.ProviderDeploymentID = "../../other" },
		"self reference":    func(c *sdkclient.ServiceBindingCredential) { c.ProviderDeploymentID = consumer },
		"database adoption": func(c *sdkclient.ServiceBindingCredential) { c.Database = "postgres" },
		"user adoption":     func(c *sdkclient.ServiceBindingCredential) { c.Username = "postgres" },
		"password SQL":      func(c *sdkclient.ServiceBindingCredential) { c.Password = "';ALTER ROLE postgres;--" },
		"admin option":      func(c *sdkclient.ServiceBindingCredential) { c.AdminUser = "--command=bad" },
		"revision":          func(c *sdkclient.ServiceBindingCredential) { c.Revision = "changed" },
		"variable newline":  func(c *sdkclient.ServiceBindingCredential) { c.Variable = "DATABASE_URL\nSECRET" },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			change(&c)
			if sql, err := postgresBindingSQL(consumer, c); err == nil || sql != "" {
				t.Fatal("unsafe binding produced SQL")
			}
		})
	}
}

func TestServiceBindingProviderConfinement(t *testing.T) {
	_, ref := bindingFixture()
	provider := ref.ProviderDeploymentID
	create := func() (bindingProviderContainer, bindingProviderNetwork) {
		c := bindingProviderContainer{ID: strings.Repeat("1", 64)}
		c.State.Running = true
		c.Config.Labels = map[string]string{"com.docker.compose.project": provider, "com.docker.compose.service": "postgres"}
		n := bindingProviderNetwork{ID: strings.Repeat("2", 64), Name: provider + "_default", Driver: "bridge", Labels: map[string]string{"com.docker.compose.project": provider, "com.docker.compose.network": "default"}, Containers: map[string]json.RawMessage{c.ID: json.RawMessage(`{}`)}}
		return c, n
	}
	c, n := create()
	if err := verifyBindingProvider(provider, []bindingProviderContainer{c}, []bindingProviderNetwork{n}); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*bindingProviderContainer, *bindingProviderNetwork){
		"container owner": func(c *bindingProviderContainer, n *bindingProviderNetwork) {
			c.Config.Labels["com.docker.compose.project"] = "foreign"
		},
		"wrong service": func(c *bindingProviderContainer, n *bindingProviderNetwork) {
			c.Config.Labels["com.docker.compose.service"] = "web"
		},
		"stopped": func(c *bindingProviderContainer, n *bindingProviderNetwork) { c.State.Running = false },
		"network owner": func(c *bindingProviderContainer, n *bindingProviderNetwork) {
			n.Labels["com.docker.compose.project"] = "foreign"
		},
		"host network":     func(c *bindingProviderContainer, n *bindingProviderNetwork) { n.Driver = "host" },
		"network name":     func(c *bindingProviderContainer, n *bindingProviderNetwork) { n.Name = "impreza-proxy" },
		"missing endpoint": func(c *bindingProviderContainer, n *bindingProviderNetwork) { n.Containers = nil },
	} {
		t.Run(name, func(t *testing.T) {
			c, n := create()
			change(&c, &n)
			if verifyBindingProvider(provider, []bindingProviderContainer{c}, []bindingProviderNetwork{n}) == nil {
				t.Fatal("foreign or invalid provider accepted")
			}
		})
	}
}
