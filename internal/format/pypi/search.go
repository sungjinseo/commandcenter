package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
)

// maxSearchCall bounds the XML-RPC call read: pip sends the terms of one
// command line.
const maxSearchCall = 64 << 10

// rpcValue is an XML-RPC value, as much of it as a search call uses.
type rpcValue struct {
	Text    string `xml:",chardata"`
	String  string `xml:"string"`
	Members []struct {
		Name  string   `xml:"name"`
		Value rpcValue `xml:"value"`
	} `xml:"struct>member"`
	Items []rpcValue `xml:"array>data>value"`
}

func (v rpcValue) str() string { return strings.TrimSpace(v.String + v.Text) }

// searchTerms reads the XML-RPC call of pip search: search({"name": [terms],
// "summary": [terms]}, "or"). The terms by field, or the reason it is another
// call, which the client is told as a fault.
func searchTerms(body io.Reader) (map[string][]*regexp.Regexp, error) {
	var call struct {
		Method string     `xml:"methodName"`
		Params []rpcValue `xml:"params>param>value"`
	}
	if err := xml.NewDecoder(body).Decode(&call); err != nil {
		return nil, fmt.Errorf("the request is not an XML-RPC call: %v", err)
	}
	if call.Method != "search" || len(call.Params) == 0 {
		return nil, fmt.Errorf("only the search method is served, with a specification")
	}
	if len(call.Params) > 1 && call.Params[1].str() != "or" {
		return nil, fmt.Errorf("only the operator or is served, not %.40q", call.Params[1].str())
	}
	terms := map[string][]*regexp.Regexp{}
	for _, m := range call.Params[0].Members {
		if m.Name != "name" && m.Name != "summary" {
			return nil, fmt.Errorf("only the fields name and summary are searched, not %.40q", m.Name)
		}
		values := m.Value.Items
		if len(values) == 0 {
			values = []rpcValue{m.Value}
		}
		for _, v := range values {
			// A term is a substring, in any case; "*" is any text.
			re, err := regexp.Compile("(?is)" + strings.ReplaceAll(regexp.QuoteMeta(v.str()), `\*`, ".*"))
			if err != nil {
				return nil, fmt.Errorf("a search term cannot be used: %v", err)
			}
			terms[m.Name] = append(terms[m.Name], re)
		}
	}
	return terms, nil
}

// search answers pip search, POST <repo>/pypi: per matching project of a
// hosted repository the versions it has. A group answers with its members',
// a project from the first member that has it unless the group merges its
// members. A proxy has nothing to say: pypi.org switched this API off.
func (h *Handler) search(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	terms, err := searchTerms(io.LimitReader(r.Body, maxSearchCall))
	var buf bytes.Buffer
	if err != nil {
		buf.WriteString(`<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><int>1</int></value></member><member><name>faultString</name><value><string>`)
		xml.EscapeText(&buf, []byte(err.Error()))
		buf.WriteString(`</string></value></member></struct></value></fault></methodResponse>`)
	} else {
		buf.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><array><data>`)
		if err := h.searchRepo(r.Context(), rp, terms, rp.MergeMembers, map[string]bool{}, &buf); err != nil {
			return err
		}
		buf.WriteString(`</data></array></value></param></params></methodResponse>`)
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Write(buf.Bytes())
	return nil
}

// searchRepo writes the hits of rp, leaving out a project seen (in a merging
// group: a version of one). ponytail: reads one core metadata file per project
// for its summary, as a row keeps none; keep the summary in the row if hosted
// repositories of many thousand projects are searched.
func (h *Handler) searchRepo(ctx context.Context, rp *repo.Repository, terms map[string][]*regexp.Regexp, merge bool, seen map[string]bool, out *bytes.Buffer) error {
	if rp.Type == "group" {
		for _, m := range rp.OnlineMembers() {
			if err := h.searchRepo(ctx, m, terms, merge, seen, out); err != nil {
				return err
			}
		}
		return nil
	}
	if rp.Type != "hosted" {
		return nil
	}
	names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return err
	}
	matches := func(field, value string) bool {
		for _, re := range terms[field] {
			if re.MatchString(value) {
				return true
			}
		}
		return false
	}
	for _, project := range names {
		if seen[project] {
			continue
		}
		pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, project)
		if err != nil {
			return err
		}
		files := map[string]string{} // version: a file of it
		var versions []string
		for _, p := range pkgs {
			var a fileAttrs
			if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
				return err
			}
			if a.Version == "" { // a row without a version: the file name's
				a.Version, _ = fileVersion(project, a.Filename)
			}
			if files[a.Version] == "" && validVersion(a.Version) { // pip parses what it gets, and stops at what it cannot
				versions = append(versions, a.Version)
				files[a.Version] = a.Filename
			}
		}
		if len(versions) == 0 {
			continue
		}
		raw, err := h.coreMetadata(ctx, rp, project, files[latestVersion(versions)])
		if err != nil && repo.StatusOf(err) != http.StatusNotFound { // no readable metadata: no summary
			return err
		}
		summary := parseCore(raw).get("summary")
		if !matches("name", project) && !matches("summary", summary) {
			continue
		}
		seen[project] = !merge
		for _, v := range versions {
			if seen[project+"\x00"+v] {
				continue
			}
			seen[project+"\x00"+v] = true
			out.WriteString(`<value><struct><member><name>_pypi_ordering</name><value><boolean>0</boolean></value></member>`)
			for _, m := range [][2]string{{"name", project}, {"version", v}, {"summary", summary}} {
				fmt.Fprintf(out, `<member><name>%s</name><value><string>`, m[0])
				xml.EscapeText(out, []byte(m[1]))
				out.WriteString(`</string></value></member>`)
			}
			out.WriteString(`</struct></value>`)
		}
	}
	return nil
}
