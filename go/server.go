// Package main: HTTP router and the REST surface the git checks depend on.
//
// Routing is hand-rolled (dispatch by method + path prefix, no regex):
//
//	GET  /healthz
//	GET  /{owner}/{repo}.git/info/refs?service=...
//	POST /{owner}/{repo}.git/git-upload-pack
//	POST /{owner}/{repo}.git/git-receive-pack
//	GET  /api/v3[/]
//	POST /api/v3/user/repos
//
// Everything else (including the /api/v3/*, /api/graphql and /mcp surfaces
// that M2-M4 build) answers the JSON 404 stub for now.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// gitPath splits a "/{owner}/{repo}.git/..." path into owner, repo and the
// remainder after ".git". ok is false when the path is not a git path.
func gitPath(path string) (owner, repo, rest string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", "", false
	}
	trimmed := strings.TrimPrefix(path, "/")
	slash := strings.IndexByte(trimmed, '/')
	if slash < 0 {
		return "", "", "", false
	}
	owner = trimmed[:slash]
	rest = trimmed[slash+1:]
	if !strings.HasSuffix(rest, ".git") && !strings.Contains(rest, ".git/") {
		return "", "", "", false
	}
	dotgit := strings.Index(rest, ".git")
	repo = rest[:dotgit]
	rest = rest[dotgit+len(".git"):]
	if repo == "" || strings.ContainsAny(owner, "/") || strings.ContainsAny(repo, "/") {
		return "", "", "", false
	}
	return owner, repo, rest, true
}

// ServeHTTP routes one request. Git repositories are looked up by name:
// the git checks create a repository through REST (under the viewer's
// token) and then address it by the viewer login, so the name is the key
// and the owner segment is accepted as given (M2 enforces ownership on
// the REST surface, where the checks test it).
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	if method == http.MethodGet && path == "/healthz" {
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if owner, repo, rest, ok := gitPath(path); ok {
		_ = owner
		repository := s.LookupRepo(repo)
		if repository == nil {
			s.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		switch {
		case method == http.MethodGet && rest == "/info/refs":
			s.handleInfoRefs(w, r, repository, r.URL.Query().Get("service"))
			return
		case method == http.MethodPost && rest == "/git-upload-pack":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				s.writeError(w, http.StatusBadRequest, "malformed request")
				return
			}
			s.handleUploadPack(w, r, repository, body)
			return
		case method == http.MethodPost && rest == "/git-receive-pack":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				s.writeError(w, http.StatusBadRequest, "malformed request")
				return
			}
			s.handleReceivePack(w, r, repository, body)
			return
		}
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}

	if path == "/api/v3" || path == "/api/v3/" {
		if method == http.MethodGet {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			s.writeJSON(w, http.StatusOK, map[string]string{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			})
			return
		}
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}

	if path == "/api/v3/user/repos" && method == http.MethodPost {
		s.handleCreateRepo(w, r)
		return
	}

	// Everything else under /api/v3/ is the REST v3 surface (M2: repos,
	// issues, pulls, contents, labels, git refs).
	if strings.HasPrefix(path, "/api/v3/") {
		s.handleRest(w, r)
		return
	}

	// GraphQL v4 (M3): POST /api/graphql, single operation or a JSON
	// array of operations.
	if path == "/api/graphql" {
		s.handleGraphQL(w, r)
		return
	}

	// MCP (M4): POST /mcp, JSON-RPC 2.0 Streamable HTTP.
	if path == "/mcp" {
		s.handleMCP(w, r)
		return
	}

	s.writeError(w, http.StatusNotFound, "Not Found")
}

// handleCreateRepo implements POST /api/v3/user/repos: the one creation path
// the rules state. A smart-HTTP push cannot invent a repository.
func (s *Store) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	if !s.ValidToken(r) {
		s.writeError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		s.writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if !ValidRepoName(input.Name) {
		s.writeError(w, http.StatusUnprocessableEntity, "Invalid repository name")
		return
	}
	repo, created := s.CreateRepo(input.Name, input.Description, input.Private)
	if !created {
		s.writeError(w, http.StatusUnprocessableEntity, "Repository already exists")
		return
	}
	s.writeJSON(w, http.StatusCreated, repoJSON(repo))
}
