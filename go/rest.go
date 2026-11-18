// Package main: REST v3 surface (M2) — repos, issues, pull requests, contents.
//
// Every reply is application/json with a non-empty X-GitHub-Request-Id;
// errors are JSON {"message": ...}. A request without the viewer's bearer
// token is 401; a repository under any other owner is 404, never 403.
// Issue and pull-request numbers share one per-repository counter, as on
// GitHub. All ids and times are deterministic (see store.go): commit ids
// come from the fixed trial instant, never the wall clock.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// -------------------------------------------------------------------------------------------
// Routing
// -------------------------------------------------------------------------------------------

// handleRest serves everything under /api/v3/ that server.go does not
// already handle (the root GET and POST /user/repos). The caller holds no
// lock; each handler below takes s.mu for the state it touches.
func (s *Store) handleRest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v3/")
	parts := splitPath(rest)

	if rest == "meta" && r.Method == http.MethodGet {
		s.writeJSON(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"})
		return
	}

	if rest == "user/repos" {
		if r.Method == http.MethodGet {
			s.handleListUserRepos(w, r)
			return
		}
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}

	if len(parts) >= 3 && parts[0] == "repos" {
		s.handleRepoRest(w, r, parts)
		return
	}

	s.writeError(w, http.StatusNotFound, "Not Found")
}

// splitPath splits a path on "/" and drops empty segments.
func splitPath(p string) []string {
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// -------------------------------------------------------------------------------------------
// Shared helpers
// -------------------------------------------------------------------------------------------

// decodeBody reads a JSON request body into dst. An empty body leaves dst
// at its zero value. A malformed body answers 400 {"message": ...}.
func (s *Store) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		s.writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	return true
}

// writeNoContent answers 204 with the JSON content type and a request id,
// and no body.
func (s *Store) writeNoContent(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.WriteHeader(http.StatusNoContent)
}

// pageParams reads page/per_page from the query: page >= 1, per_page 1..100,
// defaults 1 and 30 (GitHub's defaults).
func pageParams(q url.Values) (page, perPage int) {
	page = atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	perPage = atoiDefault(q.Get("per_page"), 30)
	if perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

func atoiDefault(text string, def int) int {
	if text == "" {
		return def
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return def
	}
	return n
}

// setLinkHeader writes GitHub's Link pagination header for the current
// request: absolute same-origin URLs, rel first/prev/next/last as they
// apply. Query parameters other than page/per_page are preserved.
func (s *Store) setLinkHeader(w http.ResponseWriter, r *http.Request, page, pages, perPage int) {
	if pages < 2 {
		return
	}
	base := "http://" + r.Host + r.URL.Path
	link := func(p int, rel string) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(perPage))
		return "<" + base + "?" + q.Encode() + `>; rel="` + rel + `"`
	}
	var rels []string
	if page > 1 {
		rels = append(rels, link(1, "first"), link(page-1, "prev"))
	}
	if page < pages {
		rels = append(rels, link(page+1, "next"), link(pages, "last"))
	}
	if len(rels) > 0 {
		w.Header().Set("Link", strings.Join(rels, ", "))
	}
}

// writeList answers one page of items as a JSON array, with the Link
// header. items must already be in reply order.
func (s *Store) writeList(w http.ResponseWriter, r *http.Request, items []any) {
	page, perPage := pageParams(r.URL.Query())
	total := len(items)
	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	s.setLinkHeader(w, r, page, pages, perPage)
	s.writeJSON(w, http.StatusOK, items[start:end])
}

// -------------------------------------------------------------------------------------------
// /user/repos
// -------------------------------------------------------------------------------------------

// handleListUserRepos answers GET /api/v3/user/repos: the viewer's
// repositories in creation order, paginated.
func (s *Store) handleListUserRepos(w http.ResponseWriter, r *http.Request) {
	if !s.ValidToken(r) {
		s.writeError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repos := make([]*Repo, 0, len(s.repos))
	for _, repo := range s.repos {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	items := make([]any, 0, len(repos))
	for _, repo := range repos {
		items = append(items, repoJSON(repo))
	}
	s.writeList(w, r, items)
}

// -------------------------------------------------------------------------------------------
// /repos/{owner}/{repo} and below
// -------------------------------------------------------------------------------------------

// handleRepoRest serves one repository-scoped request. parts[0] is "repos",
// parts[1] the owner, parts[2] the name; the rest selects the sub-resource.
func (s *Store) handleRepoRest(w http.ResponseWriter, r *http.Request, parts []string) {
	owner, name := parts[1], parts[2]
	if !s.ValidToken(r) {
		s.writeError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if owner != viewerLogin {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.repos[name]
	if repo == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	sub := parts[3:]
	switch {
	case len(sub) == 0:
		switch r.Method {
		case http.MethodGet:
			s.writeJSON(w, http.StatusOK, repoJSON(repo))
		case http.MethodPatch:
			s.handlePatchRepo(w, r, repo)
		case http.MethodDelete:
			delete(s.repos, repo.Name)
			delete(s.rest, repo.Name)
			s.writeNoContent(w)
		default:
			s.writeError(w, http.StatusNotFound, "Not Found")
		}
	case len(sub) == 1 && sub[0] == "branches" && r.Method == http.MethodGet:
		s.handleListBranches(w, r, repo)
	case len(sub) == 1 && sub[0] == "commits" && r.Method == http.MethodGet:
		s.handleListCommits(w, r, repo)
	case len(sub) == 2 && sub[0] == "git" && sub[1] == "refs" && r.Method == http.MethodPost:
		s.handleCreateRef(w, r, repo)
	case len(sub) >= 1 && sub[0] == "contents":
		s.handleContents(w, r, repo, strings.Join(sub[1:], "/"))
	case len(sub) == 1 && sub[0] == "labels" && r.Method == http.MethodPost:
		s.handleCreateLabel(w, r, repo)
	case len(sub) == 1 && sub[0] == "issues":
		switch r.Method {
		case http.MethodGet:
			s.handleListIssues(w, r, repo)
		case http.MethodPost:
			s.handleCreateIssue(w, r, repo)
		default:
			s.writeError(w, http.StatusNotFound, "Not Found")
		}
	case len(sub) == 2 && sub[0] == "issues":
		switch r.Method {
		case http.MethodGet:
			s.handleGetIssue(w, r, repo, sub[1])
		case http.MethodPatch:
			s.handlePatchIssue(w, r, repo, sub[1])
		default:
			s.writeError(w, http.StatusNotFound, "Not Found")
		}
	case len(sub) == 3 && sub[0] == "issues" && sub[2] == "comments":
		switch r.Method {
		case http.MethodGet:
			s.handleListComments(w, r, repo, sub[1])
		case http.MethodPost:
			s.handleCreateComment(w, r, repo, sub[1])
		default:
			s.writeError(w, http.StatusNotFound, "Not Found")
		}
	case len(sub) == 1 && sub[0] == "pulls":
		switch r.Method {
		case http.MethodGet:
			s.handleListPulls(w, r, repo)
		case http.MethodPost:
			s.handleCreatePull(w, r, repo)
		default:
			s.writeError(w, http.StatusNotFound, "Not Found")
		}
	case len(sub) == 2 && sub[0] == "pulls" && r.Method == http.MethodGet:
		s.handleGetPull(w, r, repo, sub[1])
	case len(sub) == 3 && sub[0] == "pulls" && sub[2] == "files" && r.Method == http.MethodGet:
		s.handleListPullFiles(w, r, repo, sub[1])
	case len(sub) == 3 && sub[0] == "pulls" && sub[2] == "merge" && r.Method == http.MethodPut:
		s.handleMergePull(w, r, repo, sub[1])
	default:
		s.writeError(w, http.StatusNotFound, "Not Found")
	}
}

// handlePatchRepo applies PATCH /repos/{o}/{r}: description (and private /
// default_branch, accepted but untested).
func (s *Store) handlePatchRepo(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var input struct {
		Description   *string `json:"description"`
		Private       *bool   `json:"private"`
		DefaultBranch *string `json:"default_branch"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	if input.Description != nil {
		repo.Description = *input.Description
	}
	if input.Private != nil {
		repo.Private = *input.Private
	}
	if input.DefaultBranch != nil && *input.DefaultBranch != "" {
		repo.DefaultBranch = *input.DefaultBranch
	}
	repo.UpdatedAt = s.NowString()
	s.writeJSON(w, http.StatusOK, repoJSON(repo))
}

// handleListBranches answers GET /repos/{o}/{r}/branches: every head ref,
// sorted by name, with its tip sha.
func (s *Store) handleListBranches(w http.ResponseWriter, r *http.Request, repo *Repo) {
	names := make([]string, 0)
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, name := range names {
		items = append(items, map[string]any{
			"name":      name,
			"commit":    map[string]any{"sha": repo.Refs["refs/heads/"+name]},
			"protected": false,
		})
	}
	s.writeJSON(w, http.StatusOK, items)
}

// handleListCommits answers GET /repos/{o}/{r}/commits[?sha=<branch|oid>]:
// the history from that tip, newest first (pre-order walk, parents after
// the commit). Without sha, the default branch tip is the start.
func (s *Store) handleListCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	shaParam := r.URL.Query().Get("sha")
	start := ""
	if shaParam == "" {
		start = repo.Refs["refs/heads/"+repo.DefaultBranch]
	} else {
		start = s.resolveRef(repo, shaParam)
	}
	if start == "" {
		if shaParam == "" {
			s.writeJSON(w, http.StatusOK, []any{})
			return
		}
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	seen := map[string]bool{}
	var order []string
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		order = append(order, oid)
		_, parents, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	items := make([]any, 0, len(order))
	for _, oid := range order {
		items = append(items, commitJSON(repo, oid, ""))
	}
	s.writeJSON(w, http.StatusOK, items)
}

// handleCreateRef answers POST /repos/{o}/{r}/git/refs: create a branch at
// a known commit.
func (s *Store) handleCreateRef(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var input struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	if !strings.HasPrefix(input.Ref, "refs/heads/") || strings.TrimPrefix(input.Ref, "refs/heads/") == "" {
		s.writeError(w, http.StatusUnprocessableEntity, "Invalid ref")
		return
	}
	if _, exists := repo.Refs[input.Ref]; exists {
		s.writeError(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	obj := repo.Objects[input.SHA]
	if obj == nil || obj.Type != objCommit {
		s.writeError(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	repo.Refs[input.Ref] = input.SHA
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"ref": input.Ref,
		"object": map[string]any{
			"sha":  input.SHA,
			"type": "commit",
		},
		"url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/git/" + input.Ref,
	})
}

// -------------------------------------------------------------------------------------------
// Git objects built by the REST surface (contents commits, squash merges)
// -------------------------------------------------------------------------------------------

// commitEpoch is the seconds-since-epoch of the fixed trial instant, so
// every commit the REST surface creates is deterministic.
func (s *Store) commitEpoch() int64 {
	if t, err := time.Parse(time.RFC3339, s.trialNow); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse(time.RFC3339, "2026-11-19T00:00:00Z"); err == nil {
		return t.Unix()
	}
	return 0
}

// gitSignature is the author/committer line every REST-made commit carries.
func (s *Store) gitSignature() string {
	return fmt.Sprintf("%s <%s@example.invalid> %d +0000", viewerLogin, viewerLogin, s.commitEpoch())
}

// makeCommit stores a commit object (tree, parents, author/committer from
// the trial instant, message) and returns its id.
func (s *Store) makeCommit(repo *Repo, treeOID string, parents []string, message string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", treeOID)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	sig := s.gitSignature()
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s\n", sig, sig, message)
	data := []byte(b.String())
	oid := HashObject(objCommit, data)
	repo.Objects[oid] = &Object{Type: objCommit, Data: data}
	return oid
}

// commitTree returns the root tree id of a commit object, or "".
func commitTree(repo *Repo, commitOID string) string {
	obj := repo.Objects[commitOID]
	if obj == nil || obj.Type != objCommit {
		return ""
	}
	tree, _, err := ParseCommit(obj.Data)
	if err != nil {
		return ""
	}
	return tree
}

// resolveRef resolves a branch name, a full ref, or a 40-hex object id to
// a commit id, or "" when it names nothing in this repository.
func (s *Store) resolveRef(repo *Repo, ref string) string {
	if IsOID(ref) {
		if repo.Objects[ref] != nil {
			return ref
		}
		return ""
	}
	if strings.HasPrefix(ref, "refs/") {
		return repo.Refs[ref]
	}
	return repo.Refs["refs/heads/"+ref]
}

// serializeTree renders tree entries in git order: by name, directories
// compared as name+"/".
func serializeTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			a += "/"
		}
		if sorted[j].Mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// storeTree stores a tree object built from entries and returns its id.
func storeTree(repo *Repo, entries []TreeEntry) string {
	data := serializeTree(entries)
	oid := HashObject(objTree, data)
	repo.Objects[oid] = &Object{Type: objTree, Data: data}
	return oid
}

// treeEntries returns the entries of a tree object, or nil.
func treeEntries(repo *Repo, treeOID string) []TreeEntry {
	if treeOID == "" {
		return nil
	}
	obj := repo.Objects[treeOID]
	if obj == nil || obj.Type != objTree {
		return nil
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return nil
	}
	return entries
}

// setTreePath returns the id of a tree like treeOID but with path parts set
// to blobOID, creating intermediate directories as needed.
func setTreePath(repo *Repo, treeOID string, parts []string, blobOID string) string {
	entries := treeEntries(repo, treeOID)
	name := parts[0]
	if len(parts) == 1 {
		found := false
		for i := range entries {
			if entries[i].Name == name {
				entries[i].Mode = "100644"
				entries[i].OID = blobOID
				found = true
			}
		}
		if !found {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
		}
		return storeTree(repo, entries)
	}
	var childOID string
	for i := range entries {
		if entries[i].Name == name && entries[i].Mode == "40000" {
			childOID = entries[i].OID
		}
	}
	childOID = setTreePath(repo, childOID, parts[1:], blobOID)
	found := false
	for i := range entries {
		if entries[i].Name == name {
			entries[i].Mode = "40000"
			entries[i].OID = childOID
			found = true
		}
	}
	if !found {
		entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: childOID})
	}
	return storeTree(repo, entries)
}

// removeTreePath returns the id of a tree like treeOID but without path
// parts; ok is false when the path is absent. Directories left empty by
// the removal are dropped, as git does.
func removeTreePath(repo *Repo, treeOID string, parts []string) (string, bool) {
	entries := treeEntries(repo, treeOID)
	name := parts[0]
	idx := -1
	for i := range entries {
		if entries[i].Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return "", false
	}
	if len(parts) == 1 {
		entries = append(entries[:idx], entries[idx+1:]...)
		return storeTree(repo, entries), true
	}
	if entries[idx].Mode != "40000" {
		return "", false
	}
	child, ok := removeTreePath(repo, entries[idx].OID, parts[1:])
	if !ok {
		return "", false
	}
	if len(treeEntries(repo, child)) == 0 {
		entries = append(entries[:idx], entries[idx+1:]...)
	} else {
		entries[idx].OID = child
	}
	return storeTree(repo, entries), true
}

// flattenTree collects path -> blob id for every file under treeOID.
func flattenTree(repo *Repo, treeOID string, prefix string, out map[string]string) {
	for _, e := range treeEntries(repo, treeOID) {
		path := prefix + e.Name
		if e.Mode == "40000" {
			flattenTree(repo, e.OID, path+"/", out)
		} else if e.Mode != "160000" {
			out[path] = e.OID
		}
	}
}

// commitJSON renders a commit the way GET /commits does. When message is
// non-empty it overrides the stored message (used by nobody yet; the
// stored message is authoritative).
func commitJSON(repo *Repo, oid string, message string) map[string]any {
	obj := repo.Objects[oid]
	if obj == nil {
		return map[string]any{"sha": oid}
	}
	tree, parents, _ := ParseCommit(obj.Data)
	if message == "" {
		message = commitMessage(obj.Data)
	}
	author, committer := commitPerson(obj.Data, "author"), commitPerson(obj.Data, "committer")
	parentList := make([]any, 0, len(parents))
	for _, p := range parents {
		parentList = append(parentList, map[string]any{"sha": p})
	}
	_ = tree
	return map[string]any{
		"sha": oid,
		"commit": map[string]any{
			"message":   message,
			"author":    author,
			"committer": committer,
		},
		"author":    map[string]any{"login": author["name"], "id": viewerID, "type": "User"},
		"committer": map[string]any{"login": committer["name"], "id": viewerID, "type": "User"},
		"parents":   parentList,
	}
}

// commitMessage extracts the message body of a commit object.
func commitMessage(data []byte) string {
	blank := bytes.Index(data, []byte("\n\n"))
	if blank < 0 {
		return ""
	}
	return strings.TrimSuffix(string(data[blank+2:]), "\n")
}

// commitPerson parses an "author Name <email> epoch tz" (or committer)
// header into GitHub's person shape.
func commitPerson(data []byte, key string) map[string]any {
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
	}
	name, email, date := viewerLogin, viewerLogin+"@example.invalid", "2026-11-19T00:00:00Z"
	for _, line := range strings.Split(string(head), "\n") {
		if !strings.HasPrefix(line, key+" ") {
			continue
		}
		rest := strings.TrimPrefix(line, key+" ")
		lt := strings.IndexByte(rest, '<')
		gt := strings.IndexByte(rest, '>')
		if lt < 0 || gt < lt {
			continue
		}
		name = strings.TrimSpace(rest[:lt])
		email = rest[lt+1 : gt]
		fields := strings.Fields(rest[gt+1:])
		if len(fields) > 0 {
			if epoch, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
				date = time.Unix(epoch, 0).UTC().Format(time.RFC3339)
			}
		}
	}
	return map[string]any{"name": name, "email": email, "date": date}
}

// countLines counts the lines of a blob's bytes (a trailing newline does
// not start a new line; empty content has zero lines).
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := strings.Count(string(data), "\n")
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// decodeB64 decodes standard base64, ignoring surrounding whitespace.
func decodeB64(text string) ([]byte, error) {
	cleaned := strings.Join(strings.Fields(text), "")
	return base64.StdEncoding.DecodeString(cleaned)
}

// -------------------------------------------------------------------------------------------
// /repos/{owner}/{repo}/contents[/{path}]
// -------------------------------------------------------------------------------------------

// handleContents dispatches GET, PUT and DELETE on a contents path.
func (s *Store) handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, filePath string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetContents(w, r, repo, filePath)
	case http.MethodPut:
		s.handlePutContents(w, r, repo, filePath)
	case http.MethodDelete:
		s.handleDeleteContents(w, r, repo, filePath)
	default:
		s.writeError(w, http.StatusNotFound, "Not Found")
	}
}

// contentFileJSON renders one file's contents reply.
func contentFileJSON(repo *Repo, filePath, sha string, data []byte) map[string]any {
	name := filePath
	if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
		name = filePath[i+1:]
	}
	return map[string]any{
		"type":         "file",
		"encoding":     "base64",
		"size":         len(data),
		"name":         name,
		"path":         filePath,
		"sha":          sha,
		"content":      base64.StdEncoding.EncodeToString(data),
		"url":          "https://github.com/" + repo.Owner + "/" + repo.Name + "/contents/" + filePath,
		"html_url":     "https://github.com/" + repo.Owner + "/" + repo.Name + "/blob/" + repo.DefaultBranch + "/" + filePath,
		"download_url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/raw/" + repo.DefaultBranch + "/" + filePath,
		"git_url":      "https://github.com/" + repo.Owner + "/" + repo.Name + "/git/blobs/" + sha,
	}
}

// dirEntries renders a directory listing: direct children of treeOID,
// files and subdirectories, sorted by name.
func dirEntries(repo *Repo, treeOID, dirPath string) []any {
	entries := treeEntries(repo, treeOID)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		path := e.Name
		if dirPath != "" {
			path = dirPath + "/" + e.Name
		}
		if e.Mode == "40000" {
			out = append(out, map[string]any{
				"type": "dir", "name": e.Name, "path": path, "sha": e.OID,
				"url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
			})
			continue
		}
		size := 0
		if obj := repo.Objects[e.OID]; obj != nil {
			size = len(obj.Data)
		}
		out = append(out, map[string]any{
			"type": "file", "name": e.Name, "path": path, "sha": e.OID, "size": size,
			"url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
		})
	}
	return out
}

// contentsTip resolves the ?ref= query (branch, ref or oid; default branch
// when absent) to a commit id, or "".
func (s *Store) contentsTip(w http.ResponseWriter, r *http.Request, repo *Repo) string {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		return repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	return s.resolveRef(repo, ref)
}

// handleGetContents answers GET on a file (base64 content + sha) or a
// directory (its entries). An empty path lists the repository root.
func (s *Store) handleGetContents(w http.ResponseWriter, r *http.Request, repo *Repo, filePath string) {
	tip := s.contentsTip(w, r, repo)
	if tip == "" {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	tree := commitTree(repo, tip)
	if filePath == "" {
		s.writeJSON(w, http.StatusOK, dirEntries(repo, tree, ""))
		return
	}
	parts := strings.Split(filePath, "/")
	cur := tree
	for i, part := range parts {
		entries := treeEntries(repo, cur)
		var hit *TreeEntry
		for j := range entries {
			if entries[j].Name == part {
				hit = &entries[j]
				break
			}
		}
		if hit == nil {
			s.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		if i == len(parts)-1 {
			if hit.Mode == "40000" {
				s.writeJSON(w, http.StatusOK, dirEntries(repo, hit.OID, filePath))
				return
			}
			obj := repo.Objects[hit.OID]
			if obj == nil {
				s.writeError(w, http.StatusNotFound, "Not Found")
				return
			}
			s.writeJSON(w, http.StatusOK, contentFileJSON(repo, filePath, hit.OID, obj.Data))
			return
		}
		if hit.Mode != "40000" {
			s.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		cur = hit.OID
	}
}

// handlePutContents answers PUT: create (201) or update (200) a file with
// a commit. A sha that does not match the file's current blob is 409; a
// create over an existing file without a sha is 422. An unborn repository
// accepts the first file on its default branch.
func (s *Store) handlePutContents(w http.ResponseWriter, r *http.Request, repo *Repo, filePath string) {
	var input struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	content, err := decodeB64(input.Content)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid base64 content")
		return
	}
	branch := input.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, exists := repo.Refs[refName]
	var treeOID string
	var parents []string
	isUpdate := false
	if exists {
		treeOID = commitTree(repo, tip)
		parents = []string{tip}
		flat := map[string]string{}
		flattenTree(repo, treeOID, "", flat)
		curSHA, present := flat[filePath]
		if input.SHA != "" {
			if !present || !strings.EqualFold(curSHA, input.SHA) {
				s.writeError(w, http.StatusConflict, "sha does not match the file's current sha")
				return
			}
			isUpdate = true
		} else if present {
			s.writeError(w, http.StatusUnprocessableEntity, "file already exists")
			return
		}
	} else {
		if branch != repo.DefaultBranch || len(repo.Refs) != 0 {
			s.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		if input.SHA != "" {
			s.writeError(w, http.StatusConflict, "sha does not match the file's current sha")
			return
		}
		treeOID = storeTree(repo, nil)
	}
	blobOID := HashObject(objBlob, content)
	repo.Objects[blobOID] = &Object{Type: objBlob, Data: content}
	newTree := setTreePath(repo, treeOID, strings.Split(filePath, "/"), blobOID)
	message := input.Message
	if message == "" {
		message = "Update " + filePath
	}
	commitSHA := s.makeCommit(repo, newTree, parents, message)
	repo.Refs[refName] = commitSHA
	repo.UpdatedAt = s.NowString()
	status := http.StatusCreated
	if isUpdate {
		status = http.StatusOK
	}
	s.writeJSON(w, status, map[string]any{
		"content": contentFileJSON(repo, filePath, blobOID, content),
		"commit": map[string]any{
			"sha":     commitSHA,
			"message": message,
			"author": map[string]any{
				"name":  viewerLogin,
				"email": viewerLogin + "@example.invalid",
				"date":  s.NowString(),
			},
			"committer": map[string]any{
				"name":  viewerLogin,
				"email": viewerLogin + "@example.invalid",
				"date":  s.NowString(),
			},
		},
	})
}

// handleDeleteContents answers DELETE: remove a file with a commit (200
// {commit:{sha}}). A sha that does not match the file's current blob is
// 409; a missing file is 404.
func (s *Store) handleDeleteContents(w http.ResponseWriter, r *http.Request, repo *Repo, filePath string) {
	var input struct {
		Message string `json:"message"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	branch := input.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, exists := repo.Refs[refName]
	if !exists {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	treeOID := commitTree(repo, tip)
	flat := map[string]string{}
	flattenTree(repo, treeOID, "", flat)
	curSHA, present := flat[filePath]
	if !present {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if input.SHA != "" && !strings.EqualFold(curSHA, input.SHA) {
		s.writeError(w, http.StatusConflict, "sha does not match the file's current sha")
		return
	}
	newTree, ok := removeTreePath(repo, treeOID, strings.Split(filePath, "/"))
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	message := input.Message
	if message == "" {
		message = "Delete " + filePath
	}
	commitSHA := s.makeCommit(repo, newTree, []string{tip}, message)
	repo.Refs[refName] = commitSHA
	repo.UpdatedAt = s.NowString()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]any{
			"sha":     commitSHA,
			"message": message,
		},
	})
}

// -------------------------------------------------------------------------------------------
// Labels
// -------------------------------------------------------------------------------------------

// labelJSON renders a label.
func labelJSON(l *Label) map[string]any {
	return map[string]any{
		"name":        l.Name,
		"color":       l.Color,
		"description": l.Description,
		"url":         "",
	}
}

// handleCreateLabel answers POST /repos/{o}/{r}/labels.
func (s *Store) handleCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var input struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	if input.Name == "" {
		s.writeError(w, http.StatusUnprocessableEntity, "Invalid label name")
		return
	}
	rr := s.restForLocked(repo)
	if _, exists := rr.Labels[input.Name]; exists {
		s.writeError(w, http.StatusUnprocessableEntity, "Label already exists")
		return
	}
	label := &Label{Name: input.Name, Color: input.Color, Description: input.Description}
	rr.Labels[input.Name] = label
	s.writeJSON(w, http.StatusCreated, labelJSON(label))
}

// -------------------------------------------------------------------------------------------
// Issues
// -------------------------------------------------------------------------------------------

// issueJSON renders an issue in the GitHub shape.
func issueJSON(s *Store, repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	rr := s.restForLocked(repo)
	for _, name := range issue.Labels {
		if l, ok := rr.Labels[name]; ok {
			labels = append(labels, labelJSON(l))
		} else {
			labels = append(labels, map[string]any{"name": name})
		}
	}
	return map[string]any{
		"id":         repo.ID*1000000 + issue.Number,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
		"comments":   len(issue.Comments),
		"html_url":   "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.FormatInt(issue.Number, 10),
	}
}

// findIssue returns the issue with this number in the repository, or nil.
func findIssue(rr *RepoRest, number int64) *Issue {
	for _, issue := range rr.Issues {
		if issue.Number == number {
			return issue
		}
	}
	return nil
}

// parseNumber parses a path segment as an issue/pull number; ok is false
// when it is not a positive integer.
func parseNumber(text string) (int64, bool) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// handleCreateIssue answers POST /repos/{o}/{r}/issues. Every named label
// must already exist in the repository, or the reply is 422.
func (s *Store) handleCreateIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var input struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	rr := s.restForLocked(repo)
	for _, name := range input.Labels {
		if _, ok := rr.Labels[name]; !ok {
			s.writeError(w, http.StatusUnprocessableEntity, "Label does not exist")
			return
		}
	}
	now := s.NowString()
	issue := &Issue{
		Number:    rr.NextNumber,
		Title:     input.Title,
		Body:      input.Body,
		State:     "open",
		Labels:    append([]string(nil), input.Labels...),
		Comments:  []*IssueComment{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	rr.NextNumber++
	rr.Issues = append(rr.Issues, issue)
	s.writeJSON(w, http.StatusCreated, issueJSON(s, repo, issue))
}

// handleListIssues answers GET /repos/{o}/{r}/issues: state filter (open,
// closed, all; default open), sort=created with direction asc/desc
// (default desc), paginated.
func (s *Store) handleListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	rr := s.restForLocked(repo)
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	direction := r.URL.Query().Get("direction")
	if direction == "" {
		direction = "desc"
	}
	list := make([]*Issue, 0, len(rr.Issues))
	for _, issue := range rr.Issues {
		if state == "all" || issue.State == state {
			list = append(list, issue)
		}
	}
	if direction == "asc" {
		sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	} else {
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	}
	items := make([]any, 0, len(list))
	for _, issue := range list {
		items = append(items, issueJSON(s, repo, issue))
	}
	s.writeList(w, r, items)
}

// handleGetIssue answers GET /repos/{o}/{r}/issues/{n}.
func (s *Store) handleGetIssue(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	issue := findIssue(s.restForLocked(repo), number)
	if issue == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSON(w, http.StatusOK, issueJSON(s, repo, issue))
}

// handlePatchIssue answers PATCH /repos/{o}/{r}/issues/{n}: state, title
// and body are updatable.
func (s *Store) handlePatchIssue(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	issue := findIssue(s.restForLocked(repo), number)
	if issue == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	var input struct {
		State *string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	if input.State != nil {
		if *input.State != "open" && *input.State != "closed" {
			s.writeError(w, http.StatusUnprocessableEntity, "Invalid state")
			return
		}
		issue.State = *input.State
	}
	if input.Title != nil {
		issue.Title = *input.Title
	}
	if input.Body != nil {
		issue.Body = *input.Body
	}
	issue.UpdatedAt = s.NowString()
	s.writeJSON(w, http.StatusOK, issueJSON(s, repo, issue))
}

// commentJSON renders one issue comment.
func commentJSON(repo *Repo, issue *Issue, c *IssueComment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
		"html_url":   "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.FormatInt(issue.Number, 10) + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
	}
}

// handleCreateComment answers POST /repos/{o}/{r}/issues/{n}/comments.
func (s *Store) handleCreateComment(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	rr := s.restForLocked(repo)
	issue := findIssue(rr, number)
	if issue == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	var input struct {
		Body string `json:"body"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	now := s.NowString()
	comment := &IssueComment{
		ID:        rr.NextCommentID,
		Body:      input.Body,
		User:      viewerLogin,
		CreatedAt: now,
		UpdatedAt: now,
	}
	rr.NextCommentID++
	issue.Comments = append(issue.Comments, comment)
	s.writeJSON(w, http.StatusCreated, commentJSON(repo, issue, comment))
}

// handleListComments answers GET /repos/{o}/{r}/issues/{n}/comments.
func (s *Store) handleListComments(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	issue := findIssue(s.restForLocked(repo), number)
	if issue == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	items := make([]any, 0, len(issue.Comments))
	for _, c := range issue.Comments {
		items = append(items, commentJSON(repo, issue, c))
	}
	s.writeList(w, r, items)
}

// -------------------------------------------------------------------------------------------
// Pull requests
// -------------------------------------------------------------------------------------------

// pullJSON renders a pull request in the GitHub shape. Before a merge,
// merged_at and merge_commit_sha are null.
func pullJSON(repo *Repo, pr *PullRequest) map[string]any {
	var mergedAt any
	var mergeSHA any
	if pr.Merged {
		mergedAt = pr.MergedAt
		mergeSHA = pr.MergeSHA
	}
	return map[string]any{
		"id":               repo.ID*1000000 + pr.Number,
		"number":           pr.Number,
		"state":            pr.State,
		"title":            pr.Title,
		"body":             pr.Body,
		"merged":           pr.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head": map[string]any{
			"ref":   pr.HeadRef,
			"sha":   pr.HeadSHA,
			"label": repo.Owner + ":" + pr.HeadRef,
			"user":  map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		},
		"base": map[string]any{
			"ref":   pr.BaseRef,
			"sha":   pr.BaseSHA,
			"label": repo.Owner + ":" + pr.BaseRef,
			"user":  map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		},
		"user":       map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at": pr.CreatedAt,
		"updated_at": pr.UpdatedAt,
		"html_url":   "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.FormatInt(pr.Number, 10),
	}
}

// findPull returns the pull request with this number, or nil.
func findPull(rr *RepoRest, number int64) *PullRequest {
	for _, pr := range rr.Pulls {
		if pr.Number == number {
			return pr
		}
	}
	return nil
}

// handleCreatePull answers POST /repos/{o}/{r}/pulls. Head and base are
// branch names; their tips at creation become head.sha and base.sha.
func (s *Store) handleCreatePull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var input struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	headSHA, okHead := repo.Refs["refs/heads/"+input.Head]
	baseSHA, okBase := repo.Refs["refs/heads/"+input.Base]
	if !okHead || !okBase {
		s.writeError(w, http.StatusUnprocessableEntity, "head or base branch does not exist")
		return
	}
	rr := s.restForLocked(repo)
	now := s.NowString()
	pr := &PullRequest{
		Number:    rr.NextNumber,
		Title:     input.Title,
		Body:      input.Body,
		State:     "open",
		HeadRef:   input.Head,
		BaseRef:   input.Base,
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		CreatedAt: now,
		UpdatedAt: now,
	}
	rr.NextNumber++
	rr.Pulls = append(rr.Pulls, pr)
	s.writeJSON(w, http.StatusCreated, pullJSON(repo, pr))
}

// handleGetPull answers GET /repos/{o}/{r}/pulls/{n}.
func (s *Store) handleGetPull(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := findPull(s.restForLocked(repo), number)
	if pr == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSON(w, http.StatusOK, pullJSON(repo, pr))
}

// handleListPulls answers GET /repos/{o}/{r}/pulls: state filter (open,
// closed, all; default open), newest first, paginated.
func (s *Store) handleListPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	rr := s.restForLocked(repo)
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	list := make([]*PullRequest, 0, len(rr.Pulls))
	for _, pr := range rr.Pulls {
		if state == "all" || pr.State == state {
			list = append(list, pr)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	items := make([]any, 0, len(list))
	for _, pr := range list {
		items = append(items, pullJSON(repo, pr))
	}
	s.writeList(w, r, items)
}

// handleListPullFiles answers GET /repos/{o}/{r}/pulls/{n}/files: the
// paths that differ between the base tip at creation and the head tip,
// with status and line counts.
func (s *Store) handleListPullFiles(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := findPull(s.restForLocked(repo), number)
	if pr == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	baseFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, pr.BaseSHA), "", baseFlat)
	headFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, pr.HeadSHA), "", headFlat)
	paths := map[string]bool{}
	for p := range baseFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	items := make([]any, 0, len(sorted))
	for _, path := range sorted {
		baseOID, inBase := baseFlat[path]
		headOID, inHead := headFlat[path]
		if inBase && inHead && baseOID == headOID {
			continue
		}
		var status string
		additions, deletions := 0, 0
		switch {
		case !inBase:
			status = "added"
			if obj := repo.Objects[headOID]; obj != nil {
				additions = countLines(obj.Data)
			}
		case !inHead:
			status = "removed"
			if obj := repo.Objects[baseOID]; obj != nil {
				deletions = countLines(obj.Data)
			}
		default:
			status = "modified"
			if obj := repo.Objects[headOID]; obj != nil {
				additions = countLines(obj.Data)
			}
			if obj := repo.Objects[baseOID]; obj != nil {
				deletions = countLines(obj.Data)
			}
		}
		items = append(items, map[string]any{
			"filename":  path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
			"sha":       headOID,
		})
	}
	s.writeJSON(w, http.StatusOK, items)
}

// handleMergePull answers PUT /repos/{o}/{r}/pulls/{n}/merge. Squash
// (merge_method=squash) writes one commit on the base branch whose tree is
// the head tree; any other method writes a two-parent merge commit with
// the same tree. A pull whose files were also changed on the base branch
// since creation is a 405 conflict; an already-merged pull is 405 too.
func (s *Store) handleMergePull(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string) {
	number, ok := parseNumber(numberText)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := findPull(s.restForLocked(repo), number)
	if pr == nil {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if pr.Merged || pr.State != "open" {
		s.writeError(w, http.StatusMethodNotAllowed, "Pull Request is already merged")
		return
	}
	var input struct {
		MergeMethod string `json:"merge_method"`
	}
	if !s.decodeBody(w, r, &input) {
		return
	}
	method := input.MergeMethod
	if method == "" {
		method = "merge"
	}

	baseRefName := "refs/heads/" + pr.BaseRef
	baseTip, ok := repo.Refs[baseRefName]
	if !ok {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	baseFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, pr.BaseSHA), "", baseFlat)
	headFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, pr.HeadSHA), "", headFlat)
	tipFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, baseTip), "", tipFlat)
	conflict := false
	for path, headOID := range headFlat {
		baseOID, inBase := baseFlat[path]
		if !inBase || baseOID == headOID {
			continue // not changed by the pull
		}
		tipOID, inTip := tipFlat[path]
		if !inTip || tipOID != baseOID {
			conflict = true // the base branch changed it too, differently
		}
	}
	for path, baseOID := range baseFlat {
		if _, inHead := headFlat[path]; inHead {
			continue
		}
		if tipOID, inTip := tipFlat[path]; inTip && tipOID != baseOID {
			conflict = true // the pull deletes a file the base changed
		}
	}
	if conflict {
		s.writeError(w, http.StatusMethodNotAllowed, "Merge conflict")
		return
	}

	var parents []string
	if method == "squash" {
		parents = []string{baseTip}
	} else {
		parents = []string{baseTip, pr.HeadSHA}
	}
	message := pr.Title
	if message == "" {
		message = "Merge pull request #" + strconv.FormatInt(pr.Number, 10)
	}
	mergeSHA := s.makeCommit(repo, commitTree(repo, pr.HeadSHA), parents, message)
	repo.Refs[baseRefName] = mergeSHA
	repo.UpdatedAt = s.NowString()
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.NowString()
	pr.MergeSHA = mergeSHA
	pr.UpdatedAt = s.NowString()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"merged":  true,
		"sha":     mergeSHA,
		"message": "Pull request successfully merged",
	})
}
