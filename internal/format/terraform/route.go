// Package terraform implements the Terraform registry protocols (modules
// v1 and providers v1, as `terraform init` and `tofu init` use them) under
// /repository/<repo>/: service discovery at .well-known/terraform.json,
// module version lists and download endpoints (204 with X-Terraform-Get),
// provider version lists and per-platform download documents, and the
// archives themselves. A hosted repository takes module archives and
// provider release sets by PUT and derives every document from its rows
// and files; a proxy caches registry.terraform.io (or another registry)
// with the archive URLs rewritten to itself and fetched on demand; a group
// answers from the first member that has the module or provider.
package terraform

import (
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
)

// Kind is the kind of repository request a path addresses.
type Kind int

const (
	KindDiscovery        Kind = iota // .well-known/terraform.json
	KindModuleVersions               // v1/modules/<ns>/<name>/<provider>/versions
	KindModuleDownload               // v1/modules/<ns>/<name>/<provider>/<version>/download
	KindModuleArchive                // modules/<ns>/<name>/<provider>/<version>.tgz or .zip (GET; PUT uploads)
	KindModuleUpload                 // v1/modules/<ns>/<name>/<provider>/<version>: PUT an archive, DELETE the version
	KindProviderVersions             // v1/providers/<ns>/<type>/versions
	KindProviderDownload             // v1/providers/<ns>/<type>/<version>/download/<os>/<arch>
	KindProviderFile                 // providers/<ns>/<type>/<version>/<file>
	KindProviderUpload               // v1/providers/<ns>/<type>/<version>/<os>/<arch> | shasums | shasums.sig | manifest (PUT), <version> (DELETE)
)

// Route is a parsed request. Name is the row name: <ns>/<name>/<provider>
// for a module, <ns>/<type> for a provider. Part is what a provider upload
// carries: "zip" (OS and Arch set), "shasums", "sig", "manifest" or ""
// (the version itself, for DELETE). Ext is a module archive's extension
// without the dot ("tgz" or "zip"); File a provider file's name.
type Route struct {
	Kind     Kind
	Name     string
	Version  string
	OS, Arch string
	Part     string
	Ext      string
	File     string
}

const (
	discoveryPath   = ".well-known/terraform.json"
	modulesPrefix   = "v1/modules/"
	providersPrefix = "v1/providers/"
	moduleStore     = "modules/"
	providerStore   = "providers/"
	ctJSON          = "application/json"
)

var (
	segRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	platRE    = regexp.MustCompile(`^[a-z0-9_]+$`)
	fileRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func ParseRoute(path string) (Route, error) {
	notFound := repo.Errorf(http.StatusNotFound, "not found: %s", path)
	if path == discoveryPath {
		return Route{Kind: KindDiscovery}, nil
	}
	segs := strings.Split(path, "/")
	ok := func(res ...bool) bool {
		for _, r := range res {
			if !r {
				return false
			}
		}
		return true
	}
	seg := func(i int) bool { return i < len(segs) && segRE.MatchString(segs[i]) }
	ver := func(i int) bool { return i < len(segs) && versionRE.MatchString(segs[i]) }
	switch {
	case strings.HasPrefix(path, modulesPrefix) && len(segs) >= 6 && ok(seg(2), seg(3), seg(4)):
		rt := Route{Name: segs[2] + "/" + segs[3] + "/" + segs[4]}
		switch {
		case len(segs) == 6 && segs[5] == "versions":
			rt.Kind = KindModuleVersions
		case len(segs) == 7 && ver(5) && segs[6] == "download":
			rt.Kind, rt.Version = KindModuleDownload, segs[5]
		case len(segs) == 6 && ver(5):
			rt.Kind, rt.Version = KindModuleUpload, segs[5]
		default:
			return Route{}, notFound
		}
		return rt, nil
	case strings.HasPrefix(path, moduleStore) && len(segs) == 5 && ok(seg(1), seg(2), seg(3)):
		rt := Route{Kind: KindModuleArchive, Name: segs[1] + "/" + segs[2] + "/" + segs[3]}
		for _, ext := range []string{"tgz", "zip"} {
			if v, cut := strings.CutSuffix(segs[4], "."+ext); cut && versionRE.MatchString(v) {
				rt.Version, rt.Ext = v, ext
				return rt, nil
			}
		}
		return Route{}, notFound
	case strings.HasPrefix(path, providersPrefix) && len(segs) >= 5 && ok(seg(2), seg(3)):
		rt := Route{Name: segs[2] + "/" + segs[3]}
		switch {
		case len(segs) == 5 && segs[4] == "versions":
			rt.Kind = KindProviderVersions
		case len(segs) == 8 && ver(4) && segs[5] == "download" && platRE.MatchString(segs[6]) && platRE.MatchString(segs[7]):
			rt.Kind, rt.Version, rt.OS, rt.Arch = KindProviderDownload, segs[4], segs[6], segs[7]
		case len(segs) == 5 && ver(4):
			rt.Kind, rt.Version = KindProviderUpload, segs[4]
		case len(segs) == 6 && ver(4) && (segs[5] == "shasums" || segs[5] == "shasums.sig" || segs[5] == "manifest"):
			rt.Kind, rt.Version, rt.Part = KindProviderUpload, segs[4], strings.TrimPrefix(segs[5], "shasums.")
		case len(segs) == 7 && ver(4) && platRE.MatchString(segs[5]) && platRE.MatchString(segs[6]):
			rt.Kind, rt.Version, rt.Part, rt.OS, rt.Arch = KindProviderUpload, segs[4], "zip", segs[5], segs[6]
		default:
			return Route{}, notFound
		}
		return rt, nil
	case strings.HasPrefix(path, providerStore) && len(segs) == 5 && ok(seg(1), seg(2), ver(3)) && fileRE.MatchString(segs[4]):
		return Route{Kind: KindProviderFile, Name: segs[1] + "/" + segs[2], Version: segs[3], File: segs[4]}, nil
	}
	return Route{}, notFound
}

// modulePath is where a module version's archive is stored.
func modulePath(name, version, ext string) string {
	return moduleStore + name + "/" + version + "." + ext
}

// providerDir is the directory of a provider version's files.
func providerDir(name, version string) string { return providerStore + name + "/" + version + "/" }

// providerType is the <type> of a provider row name <ns>/<type>.
func providerType(name string) string { _, t, _ := strings.Cut(name, "/"); return t }

// providerFile names a provider version's files as HashiCorp releases
// them: terraform-provider-<type>_<version>_<os>_<arch>.zip, and
// terraform-provider-<type>_<version>_SHA256SUMS (.sig) when os is "".
func providerFile(name, version, os, arch string) string {
	stem := "terraform-provider-" + providerType(name) + "_" + version
	if os == "" {
		return stem + "_SHA256SUMS"
	}
	return stem + "_" + os + "_" + arch + ".zip"
}

// platformOf reads os and arch out of a provider zip's file name.
func platformOf(file string) (os, arch string, ok bool) {
	stem, cut := strings.CutSuffix(file, ".zip")
	segs := strings.Split(stem, "_")
	if !cut || len(segs) < 4 {
		return "", "", false
	}
	os, arch = segs[len(segs)-2], segs[len(segs)-1]
	return os, arch, platRE.MatchString(os) && platRE.MatchString(arch)
}

// isModule reports whether k addresses a module (else a provider).
func isModule(k Kind) bool {
	return k == KindModuleVersions || k == KindModuleDownload || k == KindModuleArchive || k == KindModuleUpload
}

// DiscoveryDoc is the service discovery document that points at the
// registry at repoURL (the repository URL without a trailing slash).
func DiscoveryDoc(repoURL string) []byte {
	return []byte(`{"modules.v1":"` + repoURL + `/` + modulesPrefix + `","providers.v1":"` + repoURL + `/` + providersPrefix + `"}` + "\n")
}

// RootDiscovery is the host-root /.well-known/terraform.json (terraform
// addresses a registry by host alone): the document of the terraform
// repository named def, else of the first terraform repository of sorted
// (by name); nil when there is none. base is the base URL ("" for paths).
func RootDiscovery(sorted []*repo.Repository, def, base string) []byte {
	var first *repo.Repository
	for _, rp := range sorted {
		switch {
		case rp.Format != "terraform":
		case rp.Name == def:
			return DiscoveryDoc(base + "/repository/" + rp.Name)
		case first == nil:
			first = rp
		}
	}
	if first == nil {
		return nil
	}
	return DiscoveryDoc(base + "/repository/" + first.Name)
}
