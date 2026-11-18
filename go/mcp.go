// Package main: MCP endpoint (M4) — JSON-RPC 2.0 Streamable HTTP at POST /mcp.
//
// Protocol era: the legacy handshake (revisions 2025-11-25 and earlier),
// which the suite accepts: `server/discover` answers -32601 Method not
// found, then `initialize` + `notifications/initialized` negotiate the
// revision. Every request carries its own id and is answered under that
// id, so concurrent calls stay isolated; the tools list is a fixed
// literal, so two tools/list answers are byte-identical.
//
// Tools (all six the suite requires, by their github-mcp-server names):
// create_repo, get_repo, create_issue, list_issues, create_pull_request,
// merge_pull_request. Every tool effect goes through the same store the
// REST surface reads, under the same lock, so an MCP creation is
// immediately visible over REST with the same ids. The merge conflict
// predicate is duplicated from handleMergePull (rest.go), not refactored
// out of it, so no existing line changes.
//
// Errors: unknown method -32601, unknown tool or missing/invalid
// arguments -32602, malformed JSON body HTTP 400 + -32700, no bearer
// token HTTP 401. A tool that runs and fails (unknown repo, duplicate
// repo, merge conflict) answers a result with isError:true, never an
// HTTP 500. No clock, no randomness, no unordered map iteration on a
// request path: ids come from the store counters, times from
// MAKEFUN_TRIAL_NOW, enumerations are sorted.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
)

// mcpServerName / mcpServerVersion are the identity initialize reports.
const (
	mcpServerName    = "forge"
	mcpServerVersion = "1.0.0"
)

// mcpLegacyRevisions are the handshake-era revisions initialize accepts,
// newest first (the suite offers them in this order).
var mcpLegacyRevisions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// mcpToolProperty is one inputSchema property.
type mcpToolProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// mcpToolSchema is a tool's inputSchema: always type object.
type mcpToolSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]mcpToolProperty `json:"properties"`
	Required   []string                   `json:"required,omitempty"`
}

// mcpTool is one entry of the tools/list answer. The slice order in
// mcpTools is the wire order, fixed for the life of the process.
type mcpTool struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	InputSchema mcpToolSchema `json:"inputSchema"`
}

// mcpTools is the whole tools/list answer, in fixed order.
var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the authenticated user.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"name":        {Type: "string", Description: "Name of the repository to create."},
				"description": {Type: "string", Description: "A short description of the repository."},
				"private":     {Type: "boolean", Description: "Whether the repository is private."},
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": {Type: "string", Description: "Owner login of the repository."},
				"repo":  {Type: "string", Description: "Name of the repository."},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": {Type: "string", Description: "Owner login of the repository."},
				"repo":  {Type: "string", Description: "Name of the repository."},
				"title": {Type: "string", Description: "Title of the issue."},
				"body":  {Type: "string", Description: "Body of the issue."},
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": {Type: "string", Description: "Owner login of the repository."},
				"repo":  {Type: "string", Description: "Name of the repository."},
				"state": {Type: "string", Description: "Issue state filter: open, closed, or all."},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": {Type: "string", Description: "Owner login of the repository."},
				"repo":  {Type: "string", Description: "Name of the repository."},
				"title": {Type: "string", Description: "Title of the pull request."},
				"body":  {Type: "string", Description: "Body of the pull request."},
				"head":  {Type: "string", Description: "Name of the head branch."},
				"base":  {Type: "string", Description: "Name of the base branch."},
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge an open pull request into its base branch.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner":       {Type: "string", Description: "Owner login of the repository."},
				"repo":        {Type: "string", Description: "Name of the repository."},
				"pull_number": {Type: "number", Description: "Number of the pull request to merge."},
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

// mcpToolDef returns the tool definition for name, or nil.
func mcpToolDef(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

// -------------------------------------------------------------------------------------------
// JSON-RPC framing
// -------------------------------------------------------------------------------------------

// mcpRequest is one JSON-RPC request. ID is raw so a string or numeric id
// is echoed back verbatim; a nil ID marks a notification.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// mcpErrorObject is a JSON-RPC error object.
type mcpErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpResponse is one JSON-RPC response; Result and Error are exclusive.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpErrorObject `json:"error,omitempty"`
}

// mcpWriteJSON writes one JSON-RPC response with the JSON content type.
func (s *Store) mcpWriteJSON(w http.ResponseWriter, status int, resp mcpResponse) {
	text, err := json.Marshal(resp)
	if err != nil {
		text = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", strconv.Itoa(len(text)+1))
	w.WriteHeader(status)
	w.Write(text)
	w.Write([]byte("\n"))
}

// mcpWriteResult answers a request with a result.
func (s *Store) mcpWriteResult(w http.ResponseWriter, id json.RawMessage, result any) {
	s.mcpWriteJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// mcpWriteError answers a request with a JSON-RPC error (HTTP 200: the
// transport carries the error, the HTTP layer does not).
func (s *Store) mcpWriteError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	s.mcpWriteJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpErrorObject{Code: code, Message: message}})
}

// mcpToolResult is a tools/call result: text content plus structured
// content carrying the same object, so a client that reads either shape
// sees the ids and titles it needs.
type mcpToolResult struct {
	Content           []mcpContentBlock `json:"content"`
	StructuredContent any               `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

// mcpContentBlock is one text content block.
type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpToolValue renders value as a successful tools/call result.
func mcpToolValue(value any) mcpToolResult {
	text, err := json.Marshal(value)
	if err != nil {
		text = []byte("null")
	}
	return mcpToolResult{
		Content:           []mcpContentBlock{{Type: "text", Text: string(text)}},
		StructuredContent: value,
	}
}

// mcpToolError renders message as a failed tools/call result (isError).
func mcpToolError(message string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpContentBlock{{Type: "text", Text: message}},
		IsError: true,
	}
}

// -------------------------------------------------------------------------------------------
// Argument helpers
// -------------------------------------------------------------------------------------------

// mcpArgs is a tools/call arguments object.
type mcpArgs map[string]any

// mcpStringArg reads a string argument; ok is false when it is absent or
// not a string.
func mcpStringArg(args mcpArgs, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	text, ok := v.(string)
	return text, ok
}

// mcpBoolArg reads a boolean argument; absent is false, not an error.
func mcpBoolArg(args mcpArgs, key string) bool {
	v, ok := args[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// mcpNumberArg reads a numeric argument: a JSON number, or a string of
// digits. ok is false when it is absent or not a number.
func mcpNumberArg(args mcpArgs, key string) (int64, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case string:
		if parsed, err := strconv.ParseInt(n, 10, 64); err == nil {
			return parsed, true
		}
	case json.Number:
		if parsed, err := n.Int64(); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// -------------------------------------------------------------------------------------------
// Handler
// -------------------------------------------------------------------------------------------

// handleMCP serves POST /mcp: one JSON-RPC 2.0 request per POST, answered
// with one JSON body (no SSE stream: the client accepts a plain JSON
// answer). GET/DELETE are not part of the suite's traffic; DELETE answers
// 200 with an empty body so a legacy client's session teardown is a
// no-op, and any other method is 405.
func (s *Store) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.Header().Set("X-GitHub-Request-Id", s.RequestID())
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !s.ValidToken(r) {
		s.mcpWriteJSON(w, http.StatusUnauthorized, mcpResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpErrorObject{Code: -32000, Message: "Requires authentication"},
		})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.mcpWriteJSON(w, http.StatusBadRequest, mcpResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpErrorObject{Code: -32700, Message: "Parse error"},
		})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.mcpWriteJSON(w, http.StatusBadRequest, mcpResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpErrorObject{Code: -32700, Message: "Parse error"},
		})
		return
	}
	if req.Method == "" {
		s.mcpWriteError(w, req.ID, -32600, "Invalid Request")
		return
	}
	// A notification carries no id: notifications/initialized (and any
	// other notification) is acknowledged with an empty 202, no body.
	if len(req.ID) == 0 {
		w.Header().Set("X-GitHub-Request-Id", s.RequestID())
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "server/discover":
		// Pre-discover era: the method does not exist here, which is
		// exactly how the client detects the legacy handshake.
		s.mcpWriteError(w, req.ID, -32601, "Method not found")
	case "initialize":
		s.mcpInitialize(w, r, req)
	case "tools/list":
		s.mcpWriteResult(w, req.ID, map[string]any{"tools": mcpTools})
	case "tools/call":
		s.mcpCallTool(w, r, req)
	case "ping":
		s.mcpWriteResult(w, req.ID, map[string]any{})
	default:
		s.mcpWriteError(w, req.ID, -32601, "Method not found")
	}
}

// mcpInitialize answers the legacy handshake: the offered revision is
// echoed back when it is one the suite offers, else the newest legacy
// revision; capabilities declares tools, serverInfo names the forge.
func (s *Store) mcpInitialize(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	revision := mcpLegacyRevisions[0]
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	for _, offered := range mcpLegacyRevisions {
		if params.ProtocolVersion == offered {
			revision = offered
			break
		}
	}
	s.mcpWriteResult(w, req.ID, map[string]any{
		"protocolVersion": revision,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
	})
}

// mcpCallTool dispatches one tools/call: unknown tool and missing
// arguments are JSON-RPC -32602; a tool that runs and fails answers a
// result with isError:true.
func (s *Store) mcpCallTool(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	var params struct {
		Name      string  `json:"name"`
		Arguments mcpArgs `json:"arguments"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	if params.Arguments == nil {
		params.Arguments = mcpArgs{}
	}
	def := mcpToolDef(params.Name)
	if def == nil {
		s.mcpWriteError(w, req.ID, -32602, "Unknown tool: "+params.Name)
		return
	}
	// A required property the caller did not supply is invalid params.
	for _, key := range def.InputSchema.Required {
		if key == "pull_number" {
			if _, ok := mcpNumberArg(params.Arguments, key); !ok {
				s.mcpWriteError(w, req.ID, -32602, "Missing required argument: "+key)
				return
			}
			continue
		}
		if _, ok := mcpStringArg(params.Arguments, key); !ok {
			s.mcpWriteError(w, req.ID, -32602, "Missing required argument: "+key)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result mcpToolResult
	switch params.Name {
	case "create_repo":
		result = s.mcpCreateRepo(params.Arguments)
	case "get_repo":
		result = s.mcpGetRepo(params.Arguments)
	case "create_issue":
		result = s.mcpCreateIssue(params.Arguments)
	case "list_issues":
		result = s.mcpListIssues(params.Arguments)
	case "create_pull_request":
		result = s.mcpCreatePull(params.Arguments)
	case "merge_pull_request":
		result = s.mcpMergePull(params.Arguments)
	default:
		result = mcpToolError("Unknown tool: " + params.Name)
	}
	s.mcpWriteResult(w, req.ID, result)
}

// mcpRepoOf resolves owner/repo arguments to a repository the viewer
// owns. The caller must hold s.mu.
func (s *Store) mcpRepoOf(args mcpArgs) (*Repo, string) {
	owner, _ := mcpStringArg(args, "owner")
	if owner != viewerLogin {
		return nil, "Not Found"
	}
	name, _ := mcpStringArg(args, "repo")
	repo := s.repos[name]
	if repo == nil {
		return nil, "Not Found"
	}
	return repo, ""
}

// mcpCreateRepo implements the create_repo tool. The caller holds s.mu;
// the creation itself mirrors CreateRepo (store.go) inline so no
// lock-taking helper is called under the lock.
func (s *Store) mcpCreateRepo(args mcpArgs) mcpToolResult {
	name, _ := mcpStringArg(args, "name")
	description, _ := mcpStringArg(args, "description")
	private := mcpBoolArg(args, "private")
	if !ValidRepoName(name) {
		return mcpToolError("Invalid repository name")
	}
	if _, exists := s.repos[name]; exists {
		return mcpToolError("Repository already exists")
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
	return mcpToolValue(repoJSON(repo))
}

// mcpGetRepo implements the get_repo tool. The caller holds s.mu.
func (s *Store) mcpGetRepo(args mcpArgs) mcpToolResult {
	repo, failure := s.mcpRepoOf(args)
	if failure != "" {
		return mcpToolError(failure)
	}
	return mcpToolValue(repoJSON(repo))
}

// mcpCreateIssue implements the create_issue tool. The caller holds s.mu;
// the creation mirrors handleCreateIssue (rest.go) inline.
func (s *Store) mcpCreateIssue(args mcpArgs) mcpToolResult {
	repo, failure := s.mcpRepoOf(args)
	if failure != "" {
		return mcpToolError(failure)
	}
	title, _ := mcpStringArg(args, "title")
	body, _ := mcpStringArg(args, "body")
	if title == "" {
		return mcpToolError("Missing required argument: title")
	}
	rr := s.restForLocked(repo)
	now := s.NowString()
	issue := &Issue{
		Number:    rr.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    []string{},
		Comments:  []*IssueComment{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	rr.NextNumber++
	rr.Issues = append(rr.Issues, issue)
	return mcpToolValue(issueJSON(s, repo, issue))
}

// mcpListIssues implements the list_issues tool: newest first, filtered
// by state (open by default). The caller holds s.mu.
func (s *Store) mcpListIssues(args mcpArgs) mcpToolResult {
	repo, failure := s.mcpRepoOf(args)
	if failure != "" {
		return mcpToolError(failure)
	}
	state, _ := mcpStringArg(args, "state")
	if state == "" {
		state = "open"
	}
	rr := s.restForLocked(repo)
	list := make([]*Issue, 0, len(rr.Issues))
	for _, issue := range rr.Issues {
		if state == "all" || issue.State == state {
			list = append(list, issue)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	items := make([]any, 0, len(list))
	for _, issue := range list {
		items = append(items, issueJSON(s, repo, issue))
	}
	return mcpToolValue(items)
}

// mcpCreatePull implements the create_pull_request tool. The caller holds
// s.mu; the creation mirrors handleCreatePull (rest.go) inline.
func (s *Store) mcpCreatePull(args mcpArgs) mcpToolResult {
	repo, failure := s.mcpRepoOf(args)
	if failure != "" {
		return mcpToolError(failure)
	}
	title, _ := mcpStringArg(args, "title")
	body, _ := mcpStringArg(args, "body")
	head, _ := mcpStringArg(args, "head")
	base, _ := mcpStringArg(args, "base")
	headSHA, okHead := repo.Refs["refs/heads/"+head]
	baseSHA, okBase := repo.Refs["refs/heads/"+base]
	if !okHead || !okBase {
		return mcpToolError("head or base branch does not exist")
	}
	rr := s.restForLocked(repo)
	now := s.NowString()
	pr := &PullRequest{
		Number:    rr.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		BaseRef:   base,
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		CreatedAt: now,
		UpdatedAt: now,
	}
	rr.NextNumber++
	rr.Pulls = append(rr.Pulls, pr)
	return mcpToolValue(pullJSON(repo, pr))
}

// mcpMergePull implements the merge_pull_request tool. The caller holds
// s.mu; the conflict predicate and the merge commit mirror
// handleMergePull (rest.go) inline, merge method "merge".
func (s *Store) mcpMergePull(args mcpArgs) mcpToolResult {
	repo, failure := s.mcpRepoOf(args)
	if failure != "" {
		return mcpToolError(failure)
	}
	number, _ := mcpNumberArg(args, "pull_number")
	pr := findPull(s.restForLocked(repo), number)
	if pr == nil {
		return mcpToolError("Not Found")
	}
	if pr.Merged || pr.State != "open" {
		return mcpToolError("Pull Request is already merged")
	}
	baseRefName := "refs/heads/" + pr.BaseRef
	baseTip, ok := repo.Refs[baseRefName]
	if !ok {
		return mcpToolError("Not Found")
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
		return mcpToolError("Merge conflict")
	}
	message := pr.Title
	if message == "" {
		message = "Merge pull request #" + strconv.FormatInt(pr.Number, 10)
	}
	mergeSHA := s.makeCommit(repo, commitTree(repo, pr.HeadSHA), []string{baseTip, pr.HeadSHA}, message)
	repo.Refs[baseRefName] = mergeSHA
	repo.UpdatedAt = s.NowString()
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.NowString()
	pr.MergeSHA = mergeSHA
	pr.UpdatedAt = s.NowString()
	return mcpToolValue(map[string]any{
		"merged":  true,
		"sha":     mergeSHA,
		"message": "Pull request successfully merged",
	})
}
