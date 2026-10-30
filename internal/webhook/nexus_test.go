package webhook

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Component and asset events from the content rows of commits, as a nexus
// webhook gets them: docker tags (hosted) and manifests cached by tag
// (proxy), a flat helm directory, a Maven component written and deleted
// whole in one commit; an unchanged put is none; the initiator of work
// outside a request is *SYSTEM.
func TestNexusContentEvents(t *testing.T) {
	e := newEnv(t)
	e.d.Repos = repo.NewRegistry(nil, []config.Repository{
		{Name: "dk", Format: "docker", Type: "hosted"}, {Name: "dp", Format: "docker", Type: "hosted"},
		{Name: "helm", Format: "helm", Type: "hosted"}, {Name: "mv", Format: "maven", Type: "hosted"},
		{Name: "cn", Format: "conan", Type: "hosted"},
	})
	e.d.Node = "node-1"
	e.db.OnCommit = e.d.Changes
	rc := newReceiver(t)
	e.hook(t, meta.Webhook{Name: "n", URL: rc.URL, Secret: "k", Events: []string{"component", "asset"}, Enabled: true, Format: meta.PayloadNexus})
	e.hook(t, meta.Webhook{Name: "cc", URL: rc.URL + "/cc", Events: []string{"*"}, Enabled: true})

	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	n := 0
	put := func(repoName, p, sha string) meta.Asset {
		n++
		return meta.Asset{Repo: repoName, Path: p, BlobSHA256: sha, Size: 1, CreatedAt: t0.Add(time.Duration(n) * time.Second)}
	}
	tx := func(fn func(tx *meta.Tx) error) {
		t.Helper()
		if err := e.db.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	putAll := func(as ...meta.Asset) {
		t.Helper()
		tx(func(tx *meta.Tx) error {
			for _, a := range as {
				if err := tx.PutAsset(ctx, a); err != nil {
					return err
				}
			}
			return nil
		})
	}
	del := func(repoName string, paths ...string) {
		t.Helper()
		tx(func(tx *meta.Tx) error {
			for _, p := range paths {
				if _, err := tx.DeleteAsset(ctx, repoName, p); err != nil {
					return err
				}
			}
			return nil
		})
	}
	tag := func(image, name, version string) {
		t.Helper()
		tx(func(tx *meta.Tx) error {
			if version == "" {
				return tx.DeleteDistTag(ctx, "dk", image, name)
			}
			return tx.SetDistTag(ctx, "dk", image, name, version)
		})
	}

	tag("library/nginx", "latest", "sha256:a")
	tag("library/nginx", "latest", "sha256:a") // unchanged
	tag("library/nginx", "latest", "sha256:b")
	tag("library/nginx", "latest", "")
	putAll(put("dp", "library/nginx/manifests/1.25", "m1"), put("dp", "library/nginx/manifests/sha256:c", "m2"))
	putAll(put("helm", "mychart-1.0.0.tgz", "h1"))
	putAll(put("helm", "mychart-1.0.0.tgz", "h1")) // unchanged
	putAll(put("helm", "mychart-1.1.0.tgz", "h2"), put("helm", "index.yaml", "i1"))
	putAll(put("helm", "mychart-1.0.0.tgz", "h3"))
	del("helm", "mychart-1.0.0.tgz")
	const gav = "org/x/lib/2.0/lib-2.0"
	putAll(put("mv", gav+".jar", "j"), put("mv", gav+".pom", "p"))
	putAll(put("mv", gav+"-sources.jar", "s"))
	del("mv", gav+".jar", gav+".pom", gav+"-sources.jar")
	const rrev = "v2/conans/zlib/1.3/_/_/revisions/r1/" // a recipe and a package binary in another directory
	putAll(put("cn", rrev+"files/conanfile.py", "c1"))
	putAll(put("cn", rrev+"packages/p1/revisions/pr1/files/conan_package.tgz", "c2"))
	e.d.Emit("upload", "alice", "helm", "mychart@1.1.0", "") // a nexus webhook has it from the rows

	want := []string{
		"component dk CREATED library/nginx latest", "component dk UPDATED library/nginx latest", "component dk DELETED library/nginx latest",
		"asset dp CREATED /v2/library/nginx/manifests/1.25", "asset dp CREATED /v2/library/nginx/manifests/sha256:c",
		"component dp CREATED library/nginx 1.25",
		"asset helm CREATED /mychart-1.0.0.tgz", "component helm CREATED mychart 1.0.0",
		"asset helm CREATED /mychart-1.1.0.tgz", "asset helm CREATED /index.yaml", "component helm CREATED mychart 1.1.0",
		"asset helm UPDATED /mychart-1.0.0.tgz", "component helm UPDATED mychart 1.0.0",
		"asset helm DELETED /mychart-1.0.0.tgz", "component helm DELETED mychart 1.0.0",
		"asset mv CREATED /" + gav + ".jar", "asset mv CREATED /" + gav + ".pom", "component mv CREATED lib 2.0",
		"asset mv CREATED /" + gav + "-sources.jar", "component mv UPDATED lib 2.0",
		"asset mv DELETED /" + gav + ".jar", "asset mv DELETED /" + gav + ".pom", "asset mv DELETED /" + gav + "-sources.jar",
		"component mv DELETED lib 2.0",
		"asset cn CREATED /" + rrev + "files/conanfile.py", "component cn CREATED zlib 1.3#r1",
		"asset cn CREATED /" + rrev + "packages/p1/revisions/pr1/files/conan_package.tgz", "component cn UPDATED zlib 1.3#r1",
	}
	var got []string
	waitFor(t, "the nexus deliveries", func() bool {
		got = got[:0]
		for _, r := range rc.requests() {
			if strings.Contains(r.body, `"event"`) {
				continue // the commandcenter webhook's
			}
			if r.header.Get("X-Nexus-Webhook-Signature") != mac(sha1.New, "k", r.body) {
				t.Fatalf("signature of %s", r.body)
			}
			var p struct {
				Timestamp, NodeID, Initiator, RepositoryName, Action string
				Component                                            *struct{ Name, Group, Version, Format string }
				Asset                                                *struct{ Name, Format string }
			}
			json.Unmarshal([]byte(r.body), &p)
			if p.NodeID != "node-1" || p.Initiator != "*SYSTEM" || !strings.HasSuffix(p.Timestamp, "+00:00") {
				t.Fatalf("head: %s", r.body)
			}
			switch id := r.header.Get("X-Nexus-Webhook-Id"); {
			case id == "rm:repository:asset" && p.Asset != nil:
				got = append(got, fmt.Sprintf("asset %s %s %s", p.RepositoryName, p.Action, p.Asset.Name))
			case id == "rm:repository:component" && p.Component != nil:
				got = append(got, fmt.Sprintf("component %s %s %s %s", p.RepositoryName, p.Action, p.Component.Name, p.Component.Version))
			default:
				t.Fatalf("%s: %s", id, r.body)
			}
		}
		return len(got) >= len(want)
	})
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The commandcenter webhook got the upload event only, as before.
	time.Sleep(100 * time.Millisecond)
	var cc []string
	for _, r := range rc.requests() {
		if strings.Contains(r.body, `"event"`) {
			cc = append(cc, r.header.Get("X-Nexus-Webhook-Id"))
		}
	}
	if !slices.Equal(cc, []string{"component.created"}) {
		t.Fatalf("commandcenter deliveries: %v", cc)
	}
}

func TestNexusMatchingAndInitiator(t *testing.T) {
	cc := meta.Webhook{Enabled: true, Events: []string{"component", "repository", "audit"}}
	for kind, want := range map[string]bool{"component.created": true, "component.vulnerable": true, "repository.deleted": true,
		"user.created": true, "ping": false} {
		if matches(cc, kind, "r") != want {
			t.Errorf("alias match of %s: want %v", kind, want)
		}
	}
	nx := meta.Webhook{Enabled: true, Events: []string{"audit", "repository"}, Format: meta.PayloadNexus}
	if matches(nx, "repository.created", "r") {
		t.Error("a nexus webhook matched as a commandcenter one")
	}
	for _, c := range []struct {
		e    event
		want string
	}{
		{event{kind: "repository.created"}, "repository,audit"},
		{event{kind: "user.created"}, "audit"},
		{event{kind: "component.created", source: "upload"}, ""},
		{event{kind: "ping"}, ""},
	} {
		if got := strings.Join(nexusWants(nx, "", activityNames(c.e)...), ","); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
	scoped := meta.Webhook{Enabled: true, Events: []string{"asset"}, Format: meta.PayloadNexus, Repo: "r"}
	if len(nexusWants(scoped, "other", "asset")) != 0 || len(nexusWants(scoped, "r", "asset")) != 1 {
		t.Error("repository scope")
	}
	for _, c := range [][3]string{{"admin", "10.0.0.1", "admin/10.0.0.1"}, {"admin", "", "admin"}, {"", "10.0.0.1", "anonymous/10.0.0.1"},
		{"", "", "*SYSTEM"}, {"system", "", "*SYSTEM"}} {
		if got := initiator(c[0], c[1]); got != c[2] {
			t.Errorf("initiator(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	if got := nexusTime(time.Date(2026, 10, 30, 1, 2, 3, 4_500_000, time.FixedZone("KST", 9*3600))); got != "2026-10-29T16:02:03.004+00:00" {
		t.Errorf("nexusTime = %s", got)
	}
}
