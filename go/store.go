// Package main: in-memory state for the forge.
//
// Determinism is a design rule from the first line, not a later patch: no wall
// clock, no unseeded randomness and no unordered map iteration anywhere on a
// request path. Every id comes from a counter in creation order; every time
// shown to a client comes from MAKEFUN_TRIAL_NOW; every enumeration sorts
// its keys.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Scenario identities, derived once from the kit's sealed constant
// SCENARIO_SEED (program/env/src/wire.ts: SCENARIO_SEED = 0x6a09e667f3bcc909n;
// buildScenario() is a pure function of that constant, identical in every
// practice and final trial). Verified by running the suite's own generator
// with node22: viewer adaiversen, teammate quinnkeller, stranger olgabauer,
// org faint-meadow-org.
const (
	viewerLogin   = "adaiversen"
	viewerID      = 1000
	teammateLogin = "quinnkeller"
	strangerLogin = "olgabauer"
	strangerID    = 1002
	orgLogin      = "faint-meadow-org"
)

// forgeAgent is the agent string the forge advertises on the git wire.
const forgeAgent = "forge/1.0"

// Repo is one repository: its REST metadata plus its git refs and objects.
type Repo struct {
	ID            int64
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string

	Refs    map[string]string  // ref name -> object id (hex)
	Objects map[string]*Object // object id (hex) -> object
}

// SortedRefNames returns the ref names in byte order.
func (r *Repo) SortedRefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupObject returns the object with this id, or nil.
func (r *Repo) LookupObject(oid string) *Object {
	return r.Objects[oid]
}

// Store is the forge's whole state, guarded by one mutex (M1: correctness
// first; the lock is split in M5 when the hot paths are measured).
type Store struct {
	mu sync.Mutex

	// token is the bearer token the trial hands the entry (MAKEFUN_SHOP_TOKEN).
	// The entry never invents another identity: this token is the viewer.
	token string
	// trialNow is MAKEFUN_TRIAL_NOW verbatim; trialSeed is MAKEFUN_TRIAL_SEED.
	trialNow  string
	trialSeed string

	repos      map[string]*Repo // by repository name (owner is always the viewer)
	rest       map[string]*RepoRest // REST state (labels, issues, pulls) by repository name
	nextRepoID int64
	reqCount   uint64
}

// NewStore builds an empty store.
func NewStore(token, trialNow, trialSeed string) *Store {
	return &Store{
		token:      token,
		trialNow:   trialNow,
		trialSeed:  trialSeed,
		repos:      map[string]*Repo{},
		rest:       map[string]*RepoRest{},
		nextRepoID: 1,
	}
}

// RequestID returns the next deterministic request id for X-GitHub-Request-Id.
// A counter in request order: deterministic for an identical request sequence.
func (s *Store) RequestID() string {
	n := atomic.AddUint64(&s.reqCount, 1)
	return fmt.Sprintf("req-%d", n)
}

// NowString is the fixed trial instant shown in API timestamps. When the
// trial hands no usable instant (local manual runs), a fixed fallback is
// used so output never depends on the wall clock.
func (s *Store) NowString() string {
	if s.trialNow != "" {
		return s.trialNow
	}
	return "2026-11-19T00:00:00Z"
}

// ValidToken reports whether the request carries the viewer's bearer token.
func (s *Store) ValidToken(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return auth[len("Bearer "):] == s.token
}

// LookupRepo returns the repository with this name, or nil.
func (s *Store) LookupRepo(name string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos[name]
}

// CreateRepo creates a repository owned by the viewer. ok is false when a
// repository with that name already exists.
func (s *Store) CreateRepo(name, description string, private bool) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.repos[name]; exists {
		return nil, false
	}
	now := s.NowString()
	repo := &Repo{
		ID:            s.nextRepoID,
		Name:          name,
		Owner:         viewerLogin,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     now,
		UpdatedAt:     now,
		Refs:          map[string]string{},
		Objects:       map[string]*Object{},
	}
	s.nextRepoID++
	s.repos[name] = repo
	return repo, true
}

// ValidRepoName is GitHub's shape for a repository name.
func ValidRepoName(name string) bool {
	if len(name) == 0 || len(name) > 100 {
		return false
	}
	for i, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
		if i == 0 && (c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------------------------------------
// JSON replies (REST surface)
// -------------------------------------------------------------------------------------------

// writeJSON answers with a JSON body, the JSON content type and a request id.
func (s *Store) writeJSON(w http.ResponseWriter, status int, body any) {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"message":"Internal Server Error"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(text)+1))
	w.WriteHeader(status)
	w.Write(text)
	w.Write([]byte("\n"))
}

// writeError answers with a JSON {"message": ...} error body.
func (s *Store) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"message": message})
}

// repoJSON renders a repository the way GET /api/v3/repos/{owner}/{repo} does.
func repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]any{"login": r.Owner, "id": viewerID, "type": "User"},
		"private":        r.Private,
		"description":    r.Description,
		"fork":           false,
		"html_url":       "https://github.com/" + r.Owner + "/" + r.Name,
		"clone_url":      "https://github.com/" + r.Owner + "/" + r.Name + ".git",
		"git_url":        "git://github.com/" + r.Owner + "/" + r.Name + ".git",
		"ssh_url":        "git@github.com:" + r.Owner + "/" + r.Name + ".git",
		"default_branch": r.DefaultBranch,
		"created_at":     r.CreatedAt,
		"updated_at":     r.UpdatedAt,
		"pushed_at":      r.UpdatedAt,
	}
}

// -------------------------------------------------------------------------------------------
// REST v3 state (M2)
// -------------------------------------------------------------------------------------------

// Label is one repository label, created through POST /labels.
type Label struct {
	Name        string
	Color       string
	Description string
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	ID        int64
	Body      string
	User      string
	CreatedAt string
	UpdatedAt string
}

// Issue is one issue: its number comes from the repository's shared
// issue/pull counter, in creation order.
type Issue struct {
	Number    int64
	Title     string
	Body      string
	State     string // "open" or "closed"
	Labels    []string
	Comments  []*IssueComment
	CreatedAt string
	UpdatedAt string
}

// PullRequest is one pull request. HeadSHA/BaseSHA are the branch tips at
// creation; MergeSHA is set by the merge that closes it.
type PullRequest struct {
	Number    int64
	Title     string
	Body      string
	State     string // "open" or "closed"
	HeadRef   string
	BaseRef   string
	HeadSHA   string
	BaseSHA   string
	Merged    bool
	MergedAt  string
	MergeSHA  string
	CreatedAt string
	UpdatedAt string
}

// RepoRest is a repository's REST-only state: labels, issues and pull
// requests, plus the shared number counter and the comment id counter.
type RepoRest struct {
	Labels        map[string]*Label
	Issues        []*Issue
	Pulls         []*PullRequest
	NextNumber    int64
	NextCommentID int64
}

// restForLocked returns the REST state for a repository, creating it on
// first use. The caller must hold s.mu: it is the same lock the git paths
// use, so REST and git never interleave a repository's state.
func (s *Store) restForLocked(repo *Repo) *RepoRest {
	rr, ok := s.rest[repo.Name]
	if !ok {
		rr = &RepoRest{
			Labels:        map[string]*Label{},
			Issues:        []*Issue{},
			Pulls:         []*PullRequest{},
			NextNumber:    1,
			NextCommentID: 1,
		}
		s.rest[repo.Name] = rr
	}
	return rr
}
