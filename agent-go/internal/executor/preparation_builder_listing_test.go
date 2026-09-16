package executor

import (
	"strings"
	"testing"
)

const localBuilderJSON = `{"Current":true,"Name":"default","Driver":"docker","Nodes":[{"Name":"default","Endpoint":"default","Status":"running"}]}`

func TestPreparationBuilderListingRequiresOneLocalSelectedNode(t *testing.T) {
	for _, raw := range []string{localBuilderJSON, strings.Replace(localBuilderJSON, `"Endpoint":"default"`, `"Endpoint":"unix:///var/run/docker.sock"`, 1), localBuilderJSON + "\n" + `{"Current":false,"Name":"other","Driver":"remote"}`} {
		if err := verifyLocalBuilderListing([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"", `{`, localBuilderJSON + " garbage", localBuilderJSON + localBuilderJSON, strings.Replace(localBuilderJSON, `"Current":true,`, "", 1), strings.Replace(localBuilderJSON, `"Current":true`, `"Current":false`, 1), strings.Replace(localBuilderJSON, `"Driver":"docker"`, `"Driver":"remote"`, 1), strings.Replace(localBuilderJSON, `"Endpoint":"default"`, `"Endpoint":"tcp://remote:1234"`, 1), strings.Replace(localBuilderJSON, `"Status":"running"`, `"Status":"error"`, 1), strings.Replace(localBuilderJSON, `"Nodes":`, `"Error":"unavailable","Nodes":`, 1), strings.Replace(localBuilderJSON, `"Name":"default"`, `"Name":"foreign"`, 1), strings.Replace(localBuilderJSON, `"Nodes":[`, `"Nodes":[{"Name":"second","Endpoint":"default","Status":"running"},`, 1)} {
		if err := verifyLocalBuilderListing([]byte(raw)); err == nil {
			t.Fatalf("unsafe listing accepted: %s", raw)
		}
	}
}
