package maven

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const gaMeta1 = `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>org.example</groupId>
  <artifactId>app</artifactId>
  <versioning>
    <latest>1.1</latest>
    <release>1.1</release>
    <versions><version>1.0</version><version>1.1</version></versions>
    <lastUpdated>20260101000000</lastUpdated>
  </versioning>
</metadata>`

const gaMeta2 = `<metadata xmlns="http://maven.apache.org/METADATA/1.1.0">
  <groupId>org.example</groupId>
  <artifactId>app</artifactId>
  <versioning>
    <versions><version>1.0</version><version>2.0-SNAPSHOT</version><version>1.10</version></versions>
    <lastUpdated>20260201000000</lastUpdated>
  </versioning>
</metadata>`

const snapMetaOld = `<metadata><groupId>g</groupId><artifactId>a</artifactId><version>1.0-SNAPSHOT</version>
<versioning><snapshot><timestamp>20260101.000000</timestamp><buildNumber>1</buildNumber></snapshot>
<lastUpdated>20260101000000</lastUpdated>
<snapshotVersions>
 <snapshotVersion><extension>jar</extension><value>1.0-20260101.000000-1</value><updated>20260101000000</updated></snapshotVersion>
</snapshotVersions></versioning></metadata>`

const snapMetaNew = `<metadata><groupId>g</groupId><artifactId>a</artifactId><version>1.0-SNAPSHOT</version>
<versioning><snapshot><timestamp>20260102.000000</timestamp><buildNumber>2</buildNumber></snapshot>
<lastUpdated>20260102000000</lastUpdated>
<snapshotVersions>
 <snapshotVersion><extension>jar</extension><value>1.0-20260102.000000-2</value><updated>20260102000000</updated></snapshotVersion>
 <snapshotVersion><classifier>sources</classifier><extension>jar</extension><value>1.0-20260102.000000-2</value><updated>20260102000000</updated></snapshotVersion>
</snapshotVersions></versioning></metadata>`

func mustParse(t *testing.T, s string) *Metadata {
	t.Helper()
	m, err := ParseMetadata(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMergeGALevel(t *testing.T) {
	m := Merge(mustParse(t, gaMeta1), nil, mustParse(t, gaMeta2))
	v := m.Versioning
	if strings.Join(v.Versions, ",") != "1.0,1.1,1.10,2.0-SNAPSHOT" {
		t.Fatalf("versions = %v", v.Versions)
	}
	if v.Latest != "2.0-SNAPSHOT" || v.Release != "1.10" || v.LastUpdated != "20260201000000" {
		t.Fatalf("versioning = %+v", v)
	}
	if m.GroupID != "org.example" || m.ArtifactID != "app" {
		t.Fatalf("ids = %s:%s", m.GroupID, m.ArtifactID)
	}
}

func TestMergeSnapshotLevel(t *testing.T) {
	m := Merge(mustParse(t, snapMetaOld), mustParse(t, snapMetaNew))
	v := m.Versioning
	if m.Version != "1.0-SNAPSHOT" || v.Snapshot == nil || v.Snapshot.BuildNumber != 2 || len(v.Versions) != 0 {
		t.Fatalf("meta = %+v %+v", m, v)
	}
	if len(v.SnapshotVersions) != 2 || v.SnapshotVersions[0].Value != "1.0-20260102.000000-2" {
		t.Fatalf("snapshotVersions = %+v", v.SnapshotVersions)
	}
}

func TestMergeDeterministicEquivalentVersions(t *testing.T) {
	in := &Metadata{Versioning: &Versioning{Versions: []string{"1.0.0", "1.0", "1.0.0"}}}
	first := ""
	for i := 0; i < 20; i++ {
		got := strings.Join(Merge(in, &Metadata{Versioning: &Versioning{Versions: []string{"1.0"}}}).Versioning.Versions, ",")
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("call %d: versions = %q, first = %q", i, got, first)
		}
	}
}

func TestMergePlugins(t *testing.T) {
	a := &Metadata{Plugins: []Plugin{{Name: "A", Prefix: "a", ArtifactID: "a-plugin"}}}
	b := &Metadata{Plugins: []Plugin{{Name: "A2", Prefix: "a", ArtifactID: "x"}, {Name: "B", Prefix: "b", ArtifactID: "b-plugin"}}}
	m := Merge(a, b)
	if len(m.Plugins) != 2 || m.Plugins[0].Name != "A" || m.Versioning != nil {
		t.Fatalf("plugins = %+v versioning = %+v", m.Plugins, m.Versioning)
	}
}

func TestWriteRoundTrip(t *testing.T) {
	m := ForVersions("org.example", "app", []string{"2.0", "1.0"}, time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC))
	var buf bytes.Buffer
	if err := WriteMetadata(&buf, m); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "<?xml") || !strings.Contains(buf.String(), "<lastUpdated>20261014010203</lastUpdated>") {
		t.Fatalf("xml = %s", buf.String())
	}
	back := mustParse(t, buf.String())
	if strings.Join(back.Versioning.Versions, ",") != "1.0,2.0" || back.Versioning.Release != "2.0" {
		t.Fatalf("round trip = %+v", back.Versioning)
	}
}
