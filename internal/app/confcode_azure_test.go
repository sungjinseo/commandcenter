package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestConfigurationAzure (COMMANDCENTER_TEST_AZURITE_URL): an azure store
// travels to another instance with its account key (secrets=encrypted); a
// document that changes only its credentials applies them in place; one that
// moves it to another container is refused.
func TestConfigurationAzure(t *testing.T) {
	ep := os.Getenv("COMMANDCENTER_TEST_AZURITE_URL")
	if ep == "" {
		t.Skip("COMMANDCENTER_TEST_AZURITE_URL is not set")
	}
	ctx := context.Background()
	c := azuriteContainer(t, ep)
	h, _ := bcrypt.GenerateFromPassword([]byte("admin-password"), bcrypt.MinCost)
	src := ccStart(t, ccConfig(t, string(h)))
	src.must(t, "POST", "/-/api/blobstores", `{"name":"az","type":"azure","azure":{"accountName":"`+azuriteAccount+
		`","containerName":"`+c+`","endpoint":"`+ep+`","accountKey":"`+azuriteKey+`"}}`, 201)
	doc := src.export(t, "format=json&secrets=encrypted&include=blobStores")
	if strings.Contains(doc, azuriteKey) {
		t.Fatal("the export shows the account key")
	}

	dst := ccStart(t, ccConfig(t, string(h)))
	if st, res, b := dst.apply(t, "", doc); st != 200 || !res.OK {
		t.Fatalf("apply: %d %s", st, b)
	}
	bs, err := dst.a.DB.GetBlobStore(ctx, "az")
	if err != nil || bs.Azure == nil || bs.Azure.AccountKey != azuriteKey || bs.Azure.ContainerName != c {
		t.Fatalf("the azure store was not carried over: %+v %v", bs.Azure, err)
	}

	edit := func(f func(az map[string]any)) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		for _, s := range m["blobStores"].([]any) {
			if s := s.(map[string]any); s["name"] == "az" {
				f(s["azure"].(map[string]any))
			}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	changed := edit(func(az map[string]any) { az["clientId"] = "11111111-2222-3333-4444-555555555555" })
	if st, res, b := dst.apply(t, "", changed); st != 200 || !res.OK || res.Summary["update"] != 1 {
		t.Fatalf("credential change: %d %s", st, b)
	}
	if bs, _ := dst.a.DB.GetBlobStore(ctx, "az"); bs.Azure.ClientID != "11111111-2222-3333-4444-555555555555" || bs.Azure.AccountKey != azuriteKey {
		t.Fatalf("the credential change was not applied, or lost the key: %+v", bs.Azure)
	}
	moved := edit(func(az map[string]any) { az["containerName"] = "elsewhere" })
	if st, _, b := dst.apply(t, "dryRun=true", moved); st != 400 || !strings.Contains(b, "azure.containerName cannot be changed") {
		t.Fatalf("a container change: %d %s", st, b)
	}
}
