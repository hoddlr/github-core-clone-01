// Package main: GraphQL v4 surface (M3) — the documents gh and
// github-mcp-server actually send, nothing more.
//
// The executor is a minimal parser for those documents (not general
// GraphQL): one operation per document (or a JSON array of documents for
// GitHub's operation batching), field arguments as variables, inline
// enum lists, inline input objects, null variables, operation names and
// inline fragments (`... on Repository`). Introspection describes exactly
// the schema those documents use, through `__type` (the probes gh's
// feature detection sends) and `__schema`.
//
// Global ids are opaque and stable: base64url of "Kind:..." — never the
// plain database id or issue number — so a repeated read answers the
// same id and a mutation input round-trips to the same object. Cursors
// are base64url of "cursor:<issue number>"; pagination walks the
// creation-ordered issue list from the cursor's position, so it works
// forwards from any page.
//
// Errors are GraphQL errors: HTTP 200 with an `errors` array (an
// unknown field names the field, as the suite requires). A request
// without the viewer's bearer token is HTTP 401, as on the REST surface.
// No clock, no randomness and no unordered map iteration anywhere on a
// request path: every enumeration sorts, every id and cursor is a pure
// function of stored state.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------------------------
// Global ids and cursors (opaque, deterministic, stable across reads)
// -------------------------------------------------------------------------------------------

// gqlEncodeID renders an opaque global id for kind + parts, e.g.
// "Repository:12" -> "UmVwb3NpdG9yeToxMg==". The raw form never leaves
// the server: it is neither the database id nor the issue number.
func gqlEncodeID(kind string, parts ...string) string {
	raw := kind
	for _, p := range parts {
		raw += ":" + p
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// gqlDecodeID reverses gqlEncodeID. ok is false when the id is not one
// of ours.
func gqlDecodeID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	segs := strings.Split(string(raw), ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

// gqlRepoID is a repository's global id.
func gqlRepoID(r *Repo) string {
	return gqlEncodeID("Repository", strconv.FormatInt(r.ID, 10))
}

// gqlIssueID is an issue's global id (repository-scoped: numbers repeat
// across repositories).
func gqlIssueID(r *Repo, number int64) string {
	return gqlEncodeID("Issue", strconv.FormatInt(r.ID, 10), strconv.FormatInt(number, 10))
}

// gqlPullID is a pull request's global id.
func gqlPullID(r *Repo, number int64) string {
	return gqlEncodeID("PullRequest", strconv.FormatInt(r.ID, 10), strconv.FormatInt(number, 10))
}

// gqlCommentID is an issue comment's global id.
func gqlCommentID(r *Repo, issueNumber, commentID int64) string {
	return gqlEncodeID("IssueComment", strconv.FormatInt(r.ID, 10), strconv.FormatInt(issueNumber, 10), strconv.FormatInt(commentID, 10))
}

// gqlCursor encodes a pagination cursor for the issue with this number.
func gqlCursor(number int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(number, 10)))
}

// gqlCursorNumber decodes a cursor back to its issue number.
func gqlCursorNumber(cursor string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(text, "cursor:"), 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// -------------------------------------------------------------------------------------------
// Schema table: the types the gh / github-mcp-server documents use.
// Field lists are what __type(...).fields answers; the executor also
// validates every requested field against them.
// -------------------------------------------------------------------------------------------

type gqlTypeDef struct {
	name        string
	kind        string // OBJECT, ENUM, INPUT_OBJECT, SCALAR
	fields      []string
	enumVals    []string
	inputFields []string
}

var gqlSchema = []gqlTypeDef{
	{name: "Query", kind: "OBJECT", fields: []string{"viewer", "repository", "node", "__type", "__schema"}},
	{name: "Mutation", kind: "OBJECT", fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}},
	{name: "Repository", kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "description", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	{name: "Issue", kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt"}},
	{name: "IssueConnection", kind: "OBJECT", fields: []string{"nodes", "pageInfo", "totalCount"}},
	{name: "PullRequest", kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body", "merged", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt", "mergedAt"}},
	{name: "PullRequestConnection", kind: "OBJECT", fields: []string{"nodes", "pageInfo", "totalCount"}},
	{name: "PullRequestChangedFileConnection", kind: "OBJECT", fields: []string{"nodes", "pageInfo"}},
	{name: "PullRequestChangedFile", kind: "OBJECT", fields: []string{"path", "additions", "deletions"}},
	{name: "User", kind: "OBJECT", fields: []string{"login", "id"}},
	{name: "Ref", kind: "OBJECT", fields: []string{"name"}},
	{name: "PageInfo", kind: "OBJECT", fields: []string{"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"}},
	{name: "IssueComment", kind: "OBJECT", fields: []string{"id", "body", "createdAt", "updatedAt", "author"}},
	{name: "IssueCommentEdge", kind: "OBJECT", fields: []string{"node", "cursor"}},
	{name: "CreateIssuePayload", kind: "OBJECT", fields: []string{"issue"}},
	{name: "CloseIssuePayload", kind: "OBJECT", fields: []string{"issue"}},
	{name: "CreatePullRequestPayload", kind: "OBJECT", fields: []string{"pullRequest"}},
	{name: "MergePullRequestPayload", kind: "OBJECT", fields: []string{"pullRequest"}},
	{name: "AddCommentPayload", kind: "OBJECT", fields: []string{"commentEdge"}},
	{name: "IssueState", kind: "ENUM", enumVals: []string{"OPEN", "CLOSED"}},
	{name: "PullRequestState", kind: "ENUM", enumVals: []string{"OPEN", "CLOSED", "MERGED"}},
	{name: "MergeableState", kind: "ENUM", enumVals: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	{name: "PullRequestMergeMethod", kind: "ENUM", enumVals: []string{"MERGE", "SQUASH", "REBASE"}},
	{name: "RepositoryPermission", kind: "ENUM", enumVals: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	{name: "SearchType", kind: "ENUM", enumVals: []string{"DISCUSSION", "ISSUE", "REPOSITORY", "USER", "CODE"}},
	{name: "IssueOrderField", kind: "ENUM", enumVals: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	{name: "OrderDirection", kind: "ENUM", enumVals: []string{"ASC", "DESC"}},
	{name: "CreateIssueInput", kind: "INPUT_OBJECT", inputFields: []string{"repositoryId", "title", "body"}},
	{name: "CloseIssueInput", kind: "INPUT_OBJECT", inputFields: []string{"issueId"}},
	{name: "CreatePullRequestInput", kind: "INPUT_OBJECT", inputFields: []string{"repositoryId", "baseRefName", "headRefName", "title", "body"}},
	{name: "MergePullRequestInput", kind: "INPUT_OBJECT", inputFields: []string{"pullRequestId", "mergeMethod"}},
	{name: "AddCommentInput", kind: "INPUT_OBJECT", inputFields: []string{"subjectId", "body"}},
	{name: "IssueOrder", kind: "INPUT_OBJECT", inputFields: []string{"field", "direction"}},
	{name: "String", kind: "SCALAR"},
	{name: "Int", kind: "SCALAR"},
	{name: "ID", kind: "SCALAR"},
	{name: "Boolean", kind: "SCALAR"},
	{name: "__Schema", kind: "OBJECT", fields: []string{"queryType", "mutationType", "types", "directives"}},
	{name: "__Type", kind: "OBJECT", fields: []string{"name", "kind", "description", "fields", "enumValues", "inputFields", "possibleTypes", "interfaces", "ofType"}},
	{name: "__Field", kind: "OBJECT", fields: []string{"name", "description"}},
	{name: "__EnumValue", kind: "OBJECT", fields: []string{"name", "description"}},
	{name: "__InputValue", kind: "OBJECT", fields: []string{"name", "description", "type", "defaultValue"}},
}

// gqlTypeDefOf returns the schema entry for a type name, or nil.
func gqlTypeDefOf(name string) *gqlTypeDef {
	for i := range gqlSchema {
		if gqlSchema[i].name == name {
			return &gqlSchema[i]
		}
	}
	return nil
}

// gqlHasField reports whether typeName lists fieldName.
func gqlHasField(typeName, fieldName string) bool {
	def := gqlTypeDefOf(typeName)
	if def == nil {
		return false
	}
	for _, f := range def.fields {
		if f == fieldName {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------------------------
// Executor value wrappers: the parent object a selection set runs on.
// -------------------------------------------------------------------------------------------

type gqlRepoV struct{ r *Repo }
type gqlIssueV struct {
	r *Repo
	i *Issue
}
type gqlPRV struct {
	r *Repo
	p *PullRequest
}
type gqlUserV struct {
	login string
	id    int64
}
type gqlRefV struct{ name string }
type gqlPageInfoV struct {
	hasNext, hasPrev bool
	start, end       string
	hasStart, hasEnd bool
}
type gqlIssueConnV struct {
	r     *Repo
	items []*Issue
	page  gqlPageInfoV
	total int
}
type gqlPRConnV struct {
	r     *Repo
	items []*PullRequest
	page  gqlPageInfoV
	total int
}
type gqlFileV struct {
	path      string
	additions int
	deletions int
}
type gqlFileConnV struct {
	items []gqlFileV
	page  gqlPageInfoV
}
type gqlCommentV struct {
	r *Repo
	i *Issue
	c *IssueComment
}
type gqlCommentEdgeV struct{ node *gqlCommentV }
type gqlPayloadV struct {
	kind  string // createIssue, closeIssue, createPullRequest, mergePullRequest, addComment
	issue *gqlIssueV
	pr    *gqlPRV
	edge  *gqlCommentEdgeV
}
type gqlTypeV struct{ name string }
type gqlFieldV struct{ name string }
type gqlEnumV struct{ name string }
type gqlInputValV struct{ name string }
type gqlSchemaV struct{}

// gqlTypeNameOf answers __typename for a wrapper value.
func gqlTypeNameOf(v any) string {
	switch v.(type) {
	case *gqlRepoV:
		return "Repository"
	case *gqlIssueV:
		return "Issue"
	case *gqlPRV:
		return "PullRequest"
	case *gqlUserV:
		return "User"
	case *gqlRefV:
		return "Ref"
	case *gqlPageInfoV:
		return "PageInfo"
	case *gqlIssueConnV:
		return "IssueConnection"
	case *gqlPRConnV:
		return "PullRequestConnection"
	case *gqlFileConnV:
		return "PullRequestChangedFileConnection"
	case *gqlFileV:
		return "PullRequestChangedFile"
	case *gqlCommentV:
		return "IssueComment"
	case *gqlCommentEdgeV:
		return "IssueCommentEdge"
	case *gqlPayloadV:
		p := v.(*gqlPayloadV)
		switch p.kind {
		case "createIssue":
			return "CreateIssuePayload"
		case "closeIssue":
			return "CloseIssuePayload"
		case "createPullRequest":
			return "CreatePullRequestPayload"
		case "mergePullRequest":
			return "MergePullRequestPayload"
		default:
			return "AddCommentPayload"
		}
	case *gqlTypeV:
		return "__Type"
	case *gqlFieldV:
		return "__Field"
	case *gqlEnumV:
		return "__EnumValue"
	case *gqlInputValV:
		return "__InputValue"
	case *gqlSchemaV:
		return "__Schema"
	}
	return ""
}

// -------------------------------------------------------------------------------------------
// Lexer: names, ints, floats, strings, punctuation. Commas are
// insignificant whitespace in GraphQL and are skipped, as are `#`
// comments.
// -------------------------------------------------------------------------------------------

type gqlTok struct {
	kind string // "name", "int", "float", "string", "punct", "eof"
	val  string
}

func gqlLex(input string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(input) && input[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(input) && input[i+1] == '.' && input[i+2] == '.':
			toks = append(toks, gqlTok{kind: "punct", val: "..."})
			i += 3
		case c == '{' || c == '}' || c == '(' || c == ')' || c == ':' || c == '=' || c == '!' || c == '$' || c == '[' || c == ']' || c == '@':
			toks = append(toks, gqlTok{kind: "punct", val: string(c)})
			i++
		case c == '"':
			i++
			var b strings.Builder
			for i < len(input) && input[i] != '"' {
				if input[i] == '\\' && i+1 < len(input) {
					i++
					switch input[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case 'r':
						b.WriteByte('\r')
					case 'u':
						if i+4 < len(input) {
							if n, err := strconv.ParseUint(input[i+1:i+5], 16, 32); err == nil {
								b.WriteRune(rune(n))
							}
							i += 4
						}
					default:
						b.WriteByte(input[i])
					}
					i++
					continue
				}
				b.WriteByte(input[i])
				i++
			}
			if i >= len(input) {
				return nil, fmt.Errorf("unterminated string")
			}
			i++ // closing quote
			toks = append(toks, gqlTok{kind: "string", val: b.String()})
		case c >= '0' && c <= '9' || c == '-':
			j := i
			if input[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			if j < len(input) && input[j] == '.' {
				isFloat = true
				j++
				for j < len(input) && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			if j < len(input) && (input[j] == 'e' || input[j] == 'E') {
				isFloat = true
				j++
				if j < len(input) && (input[j] == '+' || input[j] == '-') {
					j++
				}
				for j < len(input) && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlTok{kind: kind, val: input[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(input) && (input[j] == '_' || input[j] >= 'a' && input[j] <= 'z' || input[j] >= 'A' && input[j] <= 'Z' || input[j] >= '0' && input[j] <= '9') {
				j++
			}
			toks = append(toks, gqlTok{kind: "name", val: input[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlTok{kind: "eof"})
	return toks, nil
}

// -------------------------------------------------------------------------------------------
// Parser: one document -> operations + fragment definitions.
// -------------------------------------------------------------------------------------------

// gqlVal is one parsed argument value.
type gqlVal struct {
	kind    string // "null", "bool", "string", "int", "float", "enum", "list", "object", "var"
	s       string
	b       bool
	n       float64
	list    []gqlVal
	obj     []gqlArg
	varName string
}

type gqlArg struct {
	name string
	val  gqlVal
}

// gqlSel is one field, inline fragment (`... on Type`) or fragment spread.
type gqlSel struct {
	alias    string
	name     string
	args     []gqlArg
	sels     []gqlSel
	fragOn   string // non-empty for `... on Type`
	fragName string // non-empty for `...Name`
}

type gqlVarDef struct {
	name     string
	typeName string
	def      *gqlVal // default value, when present
}

type gqlOp struct {
	kind    string // "query" or "mutation"
	name    string
	varDefs []gqlVarDef
	sels    []gqlSel
}

type gqlDoc struct {
	ops       []gqlOp
	frags     map[string][]gqlSel
	fragTypes map[string]string
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) atPunct(v string) bool {
	t := p.peek()
	return t.kind == "punct" && t.val == v
}
func (p *gqlParser) eatPunct(v string) bool {
	if p.atPunct(v) {
		p.pos++
		return true
	}
	return false
}
func (p *gqlParser) expectPunct(v string) error {
	if !p.eatPunct(v) {
		return fmt.Errorf("expected %q, got %q", v, p.peek().val)
	}
	return nil
}
func (p *gqlParser) expectName() (string, error) {
	t := p.next()
	if t.kind != "name" {
		return "", fmt.Errorf("expected a name, got %q", t.val)
	}
	return t.val, nil
}

// skipDirectives consumes `@name` / `@name(args)` sequences.
func (p *gqlParser) skipDirectives() error {
	for p.atPunct("@") {
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.atPunct("(") {
			if _, err := p.parseArgs(); err != nil {
				return err
			}
		}
	}
	return nil
}

func gqlParse(input string) (*gqlDoc, error) {
	toks, err := gqlLex(input)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDoc{frags: map[string][]gqlSel{}, fragTypes: map[string]string{}}
	for p.peek().kind != "eof" {
		if p.peek().kind == "name" && p.peek().val == "fragment" {
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			on, err := p.expectName() // "on"
			if err != nil {
				return nil, err
			}
			if on != "on" {
				return nil, fmt.Errorf("expected 'on' in fragment definition, got %q", on)
			}
			typeName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelSet()
			if err != nil {
				return nil, err
			}
			doc.frags[name] = sels
			doc.fragTypes[name] = typeName
			continue
		}
		op, err := p.parseOp()
		if err != nil {
			return nil, err
		}
		doc.ops = append(doc.ops, op)
	}
	if len(doc.ops) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return doc, nil
}

func (p *gqlParser) parseOp() (gqlOp, error) {
	var op gqlOp
	op.kind = "query"
	if p.peek().kind == "name" && (p.peek().val == "query" || p.peek().val == "mutation") {
		op.kind = p.next().val
		if p.peek().kind == "name" {
			op.name = p.next().val
		}
		if p.atPunct("(") {
			defs, err := p.parseVarDefs()
			if err != nil {
				return op, err
			}
			op.varDefs = defs
		}
		if err := p.skipDirectives(); err != nil {
			return op, err
		}
	}
	sels, err := p.parseSelSet()
	if err != nil {
		return op, err
	}
	op.sels = sels
	return op, nil
}

func (p *gqlParser) parseVarDefs() ([]gqlVarDef, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var defs []gqlVarDef
	for !p.atPunct(")") {
		if !p.eatPunct("$") {
			return nil, fmt.Errorf("expected $ in variable definition")
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		typeName, err := p.parseTypeName()
		if err != nil {
			return nil, err
		}
		def := gqlVarDef{name: name, typeName: typeName}
		if p.eatPunct("=") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			def.def = &v
		}
		defs = append(defs, def)
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return defs, nil
}

func (p *gqlParser) parseTypeName() (string, error) {
	if p.eatPunct("[") {
		inner, err := p.parseTypeName()
		if err != nil {
			return "", err
		}
		if err := p.expectPunct("]"); err != nil {
			return "", err
		}
		if p.eatPunct("!") {
			return "[" + inner + "]!", nil
		}
		return "[" + inner + "]", nil
	}
	name, err := p.expectName()
	if err != nil {
		return "", err
	}
	if p.eatPunct("!") {
		return name + "!", nil
	}
	return name, nil
}

func (p *gqlParser) parseSelSet() ([]gqlSel, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []gqlSel
	for !p.atPunct("}") {
		if p.peek().kind == "eof" {
			return nil, fmt.Errorf("unterminated selection set")
		}
		sel, err := p.parseSel()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSel() (gqlSel, error) {
	if p.atPunct("...") {
		p.next()
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			typeName, err := p.expectName()
			if err != nil {
				return gqlSel{}, err
			}
			if err := p.skipDirectives(); err != nil {
				return gqlSel{}, err
			}
			sels, err := p.parseSelSet()
			if err != nil {
				return gqlSel{}, err
			}
			return gqlSel{fragOn: typeName, sels: sels}, nil
		}
		name, err := p.expectName()
		if err != nil {
			return gqlSel{}, err
		}
		if err := p.skipDirectives(); err != nil {
			return gqlSel{}, err
		}
		return gqlSel{fragName: name}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return gqlSel{}, err
	}
	sel := gqlSel{name: name, alias: name}
	if p.eatPunct(":") {
		real, err := p.expectName()
		if err != nil {
			return gqlSel{}, err
		}
		sel.name = real
	}
	if p.atPunct("(") {
		args, err := p.parseArgs()
		if err != nil {
			return gqlSel{}, err
		}
		sel.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return gqlSel{}, err
	}
	if p.atPunct("{") {
		sels, err := p.parseSelSet()
		if err != nil {
			return gqlSel{}, err
		}
		sel.sels = sels
	}
	return sel, nil
}

func (p *gqlParser) parseArgs() ([]gqlArg, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var args []gqlArg
	for !p.atPunct(")") {
		if p.peek().kind == "eof" {
			return nil, fmt.Errorf("unterminated argument list")
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: name, val: v})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (gqlVal, error) {
	t := p.next()
	switch t.kind {
	case "punct":
		switch t.val {
		case "$":
			name, err := p.expectName()
			if err != nil {
				return gqlVal{}, err
			}
			return gqlVal{kind: "var", varName: name}, nil
		case "[":
			var list []gqlVal
			for !p.atPunct("]") {
				if p.peek().kind == "eof" {
					return gqlVal{}, fmt.Errorf("unterminated list")
				}
				v, err := p.parseValue()
				if err != nil {
					return gqlVal{}, err
				}
				list = append(list, v)
			}
			if err := p.expectPunct("]"); err != nil {
				return gqlVal{}, err
			}
			return gqlVal{kind: "list", list: list}, nil
		case "{":
			var obj []gqlArg
			for !p.atPunct("}") {
				if p.peek().kind == "eof" {
					return gqlVal{}, fmt.Errorf("unterminated input object")
				}
				name, err := p.expectName()
				if err != nil {
					return gqlVal{}, err
				}
				if err := p.expectPunct(":"); err != nil {
					return gqlVal{}, err
				}
				v, err := p.parseValue()
				if err != nil {
					return gqlVal{}, err
				}
				obj = append(obj, gqlArg{name: name, val: v})
			}
			if err := p.expectPunct("}"); err != nil {
				return gqlVal{}, err
			}
			return gqlVal{kind: "object", obj: obj}, nil
		}
		return gqlVal{}, fmt.Errorf("unexpected %q in value", t.val)
	case "string":
		return gqlVal{kind: "string", s: t.val}, nil
	case "int":
		n, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{kind: "int", n: n}, nil
	case "float":
		n, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{kind: "float", n: n}, nil
	case "name":
		switch t.val {
		case "true":
			return gqlVal{kind: "bool", b: true}, nil
		case "false":
			return gqlVal{kind: "bool", b: false}, nil
		case "null":
			return gqlVal{kind: "null"}, nil
		default:
			return gqlVal{kind: "enum", s: t.val}, nil
		}
	}
	return gqlVal{}, fmt.Errorf("unexpected token %q in value", t.val)
}

// gqlResolveVal turns a parsed value into a JSON-shaped Go value using
// the operation's variables. A variable resolves to its JSON value;
// an enum resolves to its name string.
func gqlResolveVal(v gqlVal, vars map[string]any) any {
	switch v.kind {
	case "null":
		return nil
	case "bool":
		return v.b
	case "string", "enum":
		return v.s
	case "int", "float":
		return v.n
	case "var":
		if val, ok := vars[v.varName]; ok {
			return val
		}
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, gqlResolveVal(item, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for _, a := range v.obj {
			out[a.name] = gqlResolveVal(a.val, vars)
		}
		return out
	}
	return nil
}

// -------------------------------------------------------------------------------------------
// Executor
// -------------------------------------------------------------------------------------------

type gqlErrorEntry struct {
	Message string `json:"message"`
}

// gqlExec runs one operation. The caller holds s.mu for the whole run:
// resolvers read and mutate store state directly, as the REST handlers
// do under the same lock.
type gqlExec struct {
	s     *Store
	vars  map[string]any
	frags map[string][]gqlSel
	types map[string]string
	errs  []gqlErrorEntry
	fatal bool // set when an error aborts the operation (data: null)
}

// gqlRunOp parses and runs one operation document with its variables and
// operation name, answering the GraphQL envelope.
func (s *Store) gqlRunOp(query string, vars map[string]any, operationName string) map[string]any {
	doc, err := gqlParse(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []gqlErrorEntry{{Message: err.Error()}}}
	}
	op := doc.ops[0]
	if operationName != "" {
		found := false
		for _, o := range doc.ops {
			if o.name == operationName {
				op = o
				found = true
				break
			}
		}
		if !found {
			return map[string]any{"data": nil, "errors": []gqlErrorEntry{{Message: fmt.Sprintf("unknown operation %q", operationName)}}}
		}
	}
	e := &gqlExec{s: s, vars: vars, frags: doc.frags, types: doc.fragTypes}
	for _, def := range op.varDefs {
		if _, ok := e.vars[def.name]; !ok && def.def != nil {
			e.vars[def.name] = gqlResolveVal(*def.def, vars)
		}
	}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := e.execSelSet(nil, rootType, op.sels)
	if e.fatal {
		return map[string]any{"data": nil, "errors": e.errs}
	}
	if len(e.errs) > 0 {
		return map[string]any{"data": data, "errors": e.errs}
	}
	return map[string]any{"data": data}
}

func (e *gqlExec) fail(format string, args ...any) {
	e.errs = append(e.errs, gqlErrorEntry{Message: fmt.Sprintf(format, args...)})
	e.fatal = true
}

// execSelSet runs a selection set against a parent value of parentType.
// A nil parent (a null object) answers null; a nil parent for the root
// answers an empty object.
func (e *gqlExec) execSelSet(parent any, parentType string, sels []gqlSel) map[string]any {
	out := map[string]any{}
	if parent == nil && parentType != "Query" && parentType != "Mutation" {
		return nil
	}
	for _, sel := range sels {
		if e.fatal {
			return nil
		}
		e.execSel(parent, parentType, sel, out)
	}
	return out
}

func (e *gqlExec) execSel(parent any, parentType string, sel gqlSel, out map[string]any) {
	if sel.fragOn != "" {
		if parent != nil && gqlTypeNameOf(parent) == sel.fragOn {
			for _, sub := range sel.sels {
				e.execSel(parent, parentType, sub, out)
				if e.fatal {
					return
				}
			}
		}
		return
	}
	if sel.fragName != "" {
		def, ok := e.frags[sel.fragName]
		if !ok {
			e.fail("unknown fragment %q", sel.fragName)
			return
		}
		if parent != nil && e.types[sel.fragName] != "" && gqlTypeNameOf(parent) != e.types[sel.fragName] {
			return
		}
		for _, sub := range def {
			e.execSel(parent, parentType, sub, out)
			if e.fatal {
				return
			}
		}
		return
	}
	key := sel.alias
	if sel.name == "__typename" {
		if parent == nil {
			out[key] = parentType
		} else {
			out[key] = gqlTypeNameOf(parent)
		}
		return
	}
	if !gqlHasField(parentType, sel.name) {
		e.fail("Cannot query field %q on type %q.", sel.name, parentType)
		return
	}
	args := map[string]any{}
	for _, a := range sel.args {
		args[a.name] = gqlResolveVal(a.val, e.vars)
	}
	value, childType, err := e.resolveField(parent, parentType, sel.name, args)
	if err != nil {
		e.fail("%s", err.Error())
		return
	}
	out[key] = e.shape(value, childType, sel.sels)
}

// shape renders a resolved value against its sub-selections: scalars
// pass through, objects recurse, lists map element-wise, null stays null.
func (e *gqlExec) shape(value any, childType string, sels []gqlSel) any {
	if value == nil {
		return nil
	}
	if len(sels) == 0 {
		return value
	}
	switch v := value.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if item == nil {
				out = append(out, nil)
				continue
			}
			out = append(out, e.execSelSet(item, childType, sels))
		}
		return out
	default:
		return e.execSelSet(value, childType, sels)
	}
}

// -------------------------------------------------------------------------------------------
// Argument helpers: JSON-shaped values back to Go types.
// -------------------------------------------------------------------------------------------

func gqlIntArg(args map[string]any, name string) (int64, bool) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

func gqlStringArg(args map[string]any, name string) (string, bool) {
	v, ok := args[name]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func gqlStringListArg(args map[string]any, name string) []string {
	v, ok := args[name]
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func gqlInputArg(args map[string]any, name string) map[string]any {
	v, ok := args[name]
	if !ok || v == nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// -------------------------------------------------------------------------------------------
// Store lookups shared by the resolvers (caller holds s.mu).
// -------------------------------------------------------------------------------------------

func (s *Store) gqlRepoByID(id int64) *Repo {
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// gqlRepoOfID decodes a repository global id ("Repository:<dbid>").
func (s *Store) gqlRepoOfID(id string) *Repo {
	kind, parts, ok := gqlDecodeID(id)
	if !ok || kind != "Repository" || len(parts) != 1 {
		return nil
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil
	}
	return s.gqlRepoByID(n)
}

// gqlIssueOfID decodes an issue global id ("Issue:<repo dbid>:<number>").
func (s *Store) gqlIssueOfID(id string) (*Repo, *Issue) {
	kind, parts, ok := gqlDecodeID(id)
	if !ok || kind != "Issue" || len(parts) != 2 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	number, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	repo := s.gqlRepoByID(repoID)
	if repo == nil {
		return nil, nil
	}
	return repo, findIssue(s.restForLocked(repo), number)
}

// gqlPullOfID decodes a pull-request global id.
func (s *Store) gqlPullOfID(id string) (*Repo, *PullRequest) {
	kind, parts, ok := gqlDecodeID(id)
	if !ok || kind != "PullRequest" || len(parts) != 2 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	number, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	repo := s.gqlRepoByID(repoID)
	if repo == nil {
		return nil, nil
	}
	return repo, findPull(s.restForLocked(repo), number)
}

// gqlIssueState maps a REST state to its GraphQL enum.
func gqlIssueState(state string) string {
	if state == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

// gqlPullState maps a REST pull-request state to its GraphQL enum.
func gqlPullState(pr *PullRequest) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

// gqlMergeable computes a pull request's MergeableState with the same
// conflict rule the REST merge uses: a file the pull changes that the
// base branch has also changed since creation (or deleted) conflicts.
func (s *Store) gqlMergeable(repo *Repo, pr *PullRequest) string {
	if pr.Merged || pr.State != "open" {
		return "UNKNOWN"
	}
	if s.gqlMergeConflict(repo, pr) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// gqlMergeConflict is the shared conflict predicate of the REST merge
// and the GraphQL mergeable field.
func (s *Store) gqlMergeConflict(repo *Repo, pr *PullRequest) bool {
	baseTip, ok := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return true
	}
	baseFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, pr.BaseSHA), "", baseFlat)
	headFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, pr.HeadSHA), "", headFlat)
	tipFlat := map[string]string{}
	flattenTree(repo, commitTree(repo, baseTip), "", tipFlat)
	for path, headOID := range headFlat {
		baseOID, inBase := baseFlat[path]
		if !inBase || baseOID == headOID {
			continue
		}
		tipOID, inTip := tipFlat[path]
		if !inTip || tipOID != baseOID {
			return true
		}
	}
	for path, baseOID := range baseFlat {
		if _, inHead := headFlat[path]; inHead {
			continue
		}
		if tipOID, inTip := tipFlat[path]; inTip && tipOID != baseOID {
			return true
		}
	}
	return false
}

// gqlPullFiles diffs a pull request's base tip at creation against its
// head tip, in path order, as the REST /pulls/{n}/files list does.
func (s *Store) gqlPullFiles(repo *Repo, pr *PullRequest) []gqlFileV {
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
	var out []gqlFileV
	for _, path := range sorted {
		baseOID, inBase := baseFlat[path]
		headOID, inHead := headFlat[path]
		if inBase && inHead && baseOID == headOID {
			continue
		}
		f := gqlFileV{path: path}
		switch {
		case !inBase:
			if obj := repo.Objects[headOID]; obj != nil {
				f.additions = countLines(obj.Data)
			}
		case !inHead:
			if obj := repo.Objects[baseOID]; obj != nil {
				f.deletions = countLines(obj.Data)
			}
		default:
			if obj := repo.Objects[headOID]; obj != nil {
				f.additions = countLines(obj.Data)
			}
			if obj := repo.Objects[baseOID]; obj != nil {
				f.deletions = countLines(obj.Data)
			}
		}
		out = append(out, f)
	}
	return out
}

// gqlIssuePage builds one page of an issue connection: states filter,
// orderBy (default creation order, ascending) and an after cursor that
// resumes after its issue number in the ordered list.
func (s *Store) gqlIssuePage(repo *Repo, args map[string]any) *gqlIssueConnV {
	rr := s.restForLocked(repo)
	states := gqlStringListArg(args, "states")
	keep := map[string]bool{}
	for _, st := range states {
		switch st {
		case "OPEN":
			keep["open"] = true
		case "CLOSED":
			keep["closed"] = true
		}
	}
	list := make([]*Issue, 0, len(rr.Issues))
	for _, issue := range rr.Issues {
		if len(keep) > 0 && !keep[issue.State] {
			continue
		}
		list = append(list, issue)
	}
	order := gqlInputArg(args, "orderBy")
	field, _ := order["field"].(string)
	dir, _ := order["direction"].(string)
	desc := dir == "DESC"
	if field == "" {
		// No orderBy: creation order (ascending number), the order the
		// cursor pagination check pages through.
		desc = false
	}
	sort.Slice(list, func(i, j int) bool {
		if desc {
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	total := len(list)
	start := 0
	if cursor, ok := gqlStringArg(args, "after"); ok {
		if n, ok := gqlCursorNumber(cursor); ok {
			for i, issue := range list {
				if issue.Number == n {
					start = i + 1
					break
				}
			}
		}
	}
	first, hasFirst := gqlIntArg(args, "first")
	end := len(list)
	if hasFirst && int(first) < end-start {
		end = start + int(first)
	}
	if end < start {
		end = start
	}
	page := list[start:end]
	conn := &gqlIssueConnV{r: repo, items: page, total: total}
	conn.page.hasNext = end < len(list)
	conn.page.hasPrev = start > 0
	if len(page) > 0 {
		conn.page.start = gqlCursor(page[0].Number)
		conn.page.end = gqlCursor(page[len(page)-1].Number)
		conn.page.hasStart = true
		conn.page.hasEnd = true
	}
	return conn
}

// -------------------------------------------------------------------------------------------
// Field resolution. resolveField returns the raw value, the GraphQL type
// name of an object (or element) value for sub-selection shaping, and an
// error that aborts the operation with a GraphQL error.
// -------------------------------------------------------------------------------------------

func (e *gqlExec) resolveField(parent any, parentType, field string, args map[string]any) (any, string, error) {
	switch parentType {
	case "Query":
		return e.resolveQuery(field, args)
	case "Mutation":
		return e.resolveMutation(field, args)
	case "Repository":
		return e.resolveRepository(parent.(*gqlRepoV).r, field, args)
	case "Issue":
		v := parent.(*gqlIssueV)
		return e.resolveIssue(v.r, v.i, field, args)
	case "IssueConnection":
		return e.resolveIssueConn(parent.(*gqlIssueConnV), field, args)
	case "PullRequest":
		v := parent.(*gqlPRV)
		return e.resolvePullRequest(v.r, v.p, field, args)
	case "PullRequestConnection":
		return e.resolvePRConn(parent.(*gqlPRConnV), field, args)
	case "PullRequestChangedFileConnection":
		return e.resolveFileConn(parent.(*gqlFileConnV), field, args)
	case "PullRequestChangedFile":
		return e.resolveFile(parent.(*gqlFileV), field, args)
	case "User":
		return e.resolveUser(parent.(*gqlUserV), field, args)
	case "Ref":
		return parent.(*gqlRefV).name, "", nil
	case "PageInfo":
		return e.resolvePageInfo(parent.(*gqlPageInfoV), field, args)
	case "IssueComment":
		v := parent.(*gqlCommentV)
		return e.resolveComment(v, field, args)
	case "IssueCommentEdge":
		edge := parent.(*gqlCommentEdgeV)
		switch field {
		case "node":
			return edge.node, "IssueComment", nil
		case "cursor":
			return gqlCommentID(edge.node.r, edge.node.i.Number, edge.node.c.ID), "", nil
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		p := parent.(*gqlPayloadV)
		if field == "issue" {
			return p.issue, "Issue", nil
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		p := parent.(*gqlPayloadV)
		if field == "pullRequest" {
			return p.pr, "PullRequest", nil
		}
	case "AddCommentPayload":
		p := parent.(*gqlPayloadV)
		if field == "commentEdge" {
			return p.edge, "IssueCommentEdge", nil
		}
	case "__Type":
		return e.resolveType(parent.(*gqlTypeV), field, args)
	case "__Field":
		if field == "name" {
			return parent.(*gqlFieldV).name, "", nil
		}
		return nil, "", nil
	case "__EnumValue":
		if field == "name" {
			return parent.(*gqlEnumV).name, "", nil
		}
		return nil, "", nil
	case "__InputValue":
		if field == "name" {
			return parent.(*gqlInputValV).name, "", nil
		}
		return nil, "", nil
	case "__Schema":
		return e.resolveSchema(field, args)
	}
	return nil, "", fmt.Errorf("cannot resolve field %q on type %q", field, parentType)
}

// -------------------------------------------------------------------------------------------
// Query
// -------------------------------------------------------------------------------------------

func (e *gqlExec) resolveQuery(field string, args map[string]any) (any, string, error) {
	switch field {
	case "viewer":
		return &gqlUserV{login: viewerLogin, id: viewerID}, "User", nil
	case "repository":
		owner, _ := gqlStringArg(args, "owner")
		name, _ := gqlStringArg(args, "name")
		// Any owner but the viewer reads as a missing repository (the
		// REST surface answers 404 for another owner's repository;
		// GraphQL answers null, never an error).
		if owner != viewerLogin {
			return nil, "Repository", nil
		}
		repo := e.s.repos[name]
		if repo == nil {
			return nil, "Repository", nil
		}
		return &gqlRepoV{r: repo}, "Repository", nil
	case "node":
		id, _ := gqlStringArg(args, "id")
		kind, parts, ok := gqlDecodeID(id)
		if !ok {
			return nil, "", nil
		}
		switch kind {
		case "Repository":
			if len(parts) != 1 {
				return nil, "", nil
			}
			n, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				return nil, "", nil
			}
			repo := e.s.gqlRepoByID(n)
			if repo == nil {
				return nil, "Repository", nil
			}
			return &gqlRepoV{r: repo}, "Repository", nil
		case "Issue":
			repo, issue := e.s.gqlIssueOfID(id)
			if issue == nil {
				return nil, "Issue", nil
			}
			return &gqlIssueV{r: repo, i: issue}, "Issue", nil
		case "PullRequest":
			repo, pr := e.s.gqlPullOfID(id)
			if pr == nil {
				return nil, "PullRequest", nil
			}
			return &gqlPRV{r: repo, p: pr}, "PullRequest", nil
		}
		return nil, "", nil
	case "__type":
		name, _ := gqlStringArg(args, "name")
		if gqlTypeDefOf(name) == nil {
			return nil, "__Type", nil
		}
		return &gqlTypeV{name: name}, "__Type", nil
	case "__schema":
		return &gqlSchemaV{}, "__Schema", nil
	}
	return nil, "", fmt.Errorf("unknown Query field %q", field)
}

// -------------------------------------------------------------------------------------------
// Mutation: the same store writes the REST surface makes, so a GraphQL
// mutation is visible to REST immediately and vice versa.
// -------------------------------------------------------------------------------------------

func (e *gqlExec) resolveMutation(field string, args map[string]any) (any, string, error) {
	input := gqlInputArg(args, "input")
	switch field {
	case "createIssue":
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		repo := e.s.gqlRepoOfID(repoID)
		if repo == nil {
			return nil, "", fmt.Errorf("could not find repository %q", repoID)
		}
		rr := e.s.restForLocked(repo)
		now := e.s.NowString()
		issue := &Issue{Number: rr.NextNumber, Title: title, Body: body, State: "open", Comments: []*IssueComment{}, CreatedAt: now, UpdatedAt: now}
		rr.NextNumber++
		rr.Issues = append(rr.Issues, issue)
		return &gqlPayloadV{kind: "createIssue", issue: &gqlIssueV{r: repo, i: issue}}, "CreateIssuePayload", nil
	case "closeIssue":
		issueID, _ := input["issueId"].(string)
		repo, issue := e.s.gqlIssueOfID(issueID)
		if issue == nil {
			return nil, "", fmt.Errorf("could not find issue %q", issueID)
		}
		issue.State = "closed"
		issue.UpdatedAt = e.s.NowString()
		return &gqlPayloadV{kind: "closeIssue", issue: &gqlIssueV{r: repo, i: issue}}, "CloseIssuePayload", nil
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		baseRef, _ := input["baseRefName"].(string)
		headRef, _ := input["headRefName"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		repo := e.s.gqlRepoOfID(repoID)
		if repo == nil {
			return nil, "", fmt.Errorf("could not find repository %q", repoID)
		}
		headSHA, okHead := repo.Refs["refs/heads/"+headRef]
		baseSHA, okBase := repo.Refs["refs/heads/"+baseRef]
		if !okHead || !okBase {
			return nil, "", fmt.Errorf("head or base branch does not exist")
		}
		rr := e.s.restForLocked(repo)
		now := e.s.NowString()
		pr := &PullRequest{Number: rr.NextNumber, Title: title, Body: body, State: "open", HeadRef: headRef, BaseRef: baseRef, HeadSHA: headSHA, BaseSHA: baseSHA, CreatedAt: now, UpdatedAt: now}
		rr.NextNumber++
		rr.Pulls = append(rr.Pulls, pr)
		return &gqlPayloadV{kind: "createPullRequest", pr: &gqlPRV{r: repo, p: pr}}, "CreatePullRequestPayload", nil
	case "mergePullRequest":
		prID, _ := input["pullRequestId"].(string)
		method, _ := input["mergeMethod"].(string)
		repo, pr := e.s.gqlPullOfID(prID)
		if pr == nil {
			return nil, "", fmt.Errorf("could not find pull request %q", prID)
		}
		if pr.Merged || pr.State != "open" {
			return nil, "", fmt.Errorf("pull request is already merged")
		}
		if e.s.gqlMergeConflict(repo, pr) {
			return nil, "", fmt.Errorf("merge conflict")
		}
		baseRefName := "refs/heads/" + pr.BaseRef
		baseTip, ok := repo.Refs[baseRefName]
		if !ok {
			return nil, "", fmt.Errorf("base branch does not exist")
		}
		var parents []string
		if method == "SQUASH" {
			parents = []string{baseTip}
		} else {
			parents = []string{baseTip, pr.HeadSHA}
		}
		message := pr.Title
		if message == "" {
			message = "Merge pull request #" + strconv.FormatInt(pr.Number, 10)
		}
		mergeSHA := e.s.makeCommit(repo, commitTree(repo, pr.HeadSHA), parents, message)
		repo.Refs[baseRefName] = mergeSHA
		repo.UpdatedAt = e.s.NowString()
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = e.s.NowString()
		pr.MergeSHA = mergeSHA
		pr.UpdatedAt = e.s.NowString()
		return &gqlPayloadV{kind: "mergePullRequest", pr: &gqlPRV{r: repo, p: pr}}, "MergePullRequestPayload", nil
	case "addComment":
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		repo, issue := e.s.gqlIssueOfID(subjectID)
		if issue == nil {
			return nil, "", fmt.Errorf("could not find issue %q", subjectID)
		}
		rr := e.s.restForLocked(repo)
		now := e.s.NowString()
		comment := &IssueComment{ID: rr.NextCommentID, Body: body, User: viewerLogin, CreatedAt: now, UpdatedAt: now}
		rr.NextCommentID++
		issue.Comments = append(issue.Comments, comment)
		edge := &gqlCommentEdgeV{node: &gqlCommentV{r: repo, i: issue, c: comment}}
		return &gqlPayloadV{kind: "addComment", edge: edge}, "AddCommentPayload", nil
	}
	return nil, "", fmt.Errorf("unknown Mutation field %q", field)
}

// -------------------------------------------------------------------------------------------
// Repository, Issue, PullRequest
// -------------------------------------------------------------------------------------------

func (e *gqlExec) resolveRepository(repo *Repo, field string, args map[string]any) (any, string, error) {
	switch field {
	case "id":
		return gqlRepoID(repo), "", nil
	case "databaseId":
		return repo.ID, "", nil
	case "name":
		return repo.Name, "", nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, "", nil
	case "description":
		return repo.Description, "", nil
	case "owner":
		return &gqlUserV{login: repo.Owner, id: viewerID}, "User", nil
	case "defaultBranchRef":
		return &gqlRefV{name: repo.DefaultBranch}, "Ref", nil
	case "viewerPermission":
		return "ADMIN", "", nil
	case "hasIssuesEnabled":
		return true, "", nil
	case "issue":
		number, ok := gqlIntArg(args, "number")
		if !ok {
			return nil, "Issue", nil
		}
		issue := findIssue(e.s.restForLocked(repo), number)
		if issue == nil {
			return nil, "Issue", nil
		}
		return &gqlIssueV{r: repo, i: issue}, "Issue", nil
	case "issues":
		return e.s.gqlIssuePage(repo, args), "IssueConnection", nil
	case "pullRequests":
		rr := e.s.restForLocked(repo)
		states := gqlStringListArg(args, "states")
		keep := map[string]bool{}
		for _, st := range states {
			switch st {
			case "OPEN":
				keep["open"] = true
			case "CLOSED":
				keep["closed"] = true
			case "MERGED":
				keep["merged"] = true
			}
		}
		list := make([]*PullRequest, 0, len(rr.Pulls))
		for _, pr := range rr.Pulls {
			state := pr.State
			if pr.Merged {
				state = "merged"
			}
			if len(keep) > 0 && !keep[state] {
				continue
			}
			// No states argument: the open list, as gh's `gh pr list` reads.
			if len(keep) == 0 && pr.State != "open" {
				continue
			}
			list = append(list, pr)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
		total := len(list)
		start := 0
		if cursor, ok := gqlStringArg(args, "after"); ok {
			if n, ok := gqlCursorNumber(cursor); ok {
				for i, pr := range list {
					if pr.Number == n {
						start = i + 1
						break
					}
				}
			}
		}
		end := len(list)
		if first, hasFirst := gqlIntArg(args, "first"); hasFirst && int(first) < end-start {
			end = start + int(first)
		}
		if end < start {
			end = start
		}
		page := list[start:end]
		conn := &gqlPRConnV{r: repo, items: page, total: total}
		conn.page.hasNext = end < len(list)
		conn.page.hasPrev = start > 0
		if len(page) > 0 {
			conn.page.start = gqlCursor(page[0].Number)
			conn.page.end = gqlCursor(page[len(page)-1].Number)
			conn.page.hasStart = true
			conn.page.hasEnd = true
		}
		return conn, "PullRequestConnection", nil
	case "pullRequest":
		number, ok := gqlIntArg(args, "number")
		if !ok {
			return nil, "PullRequest", nil
		}
		pr := findPull(e.s.restForLocked(repo), number)
		if pr == nil {
			return nil, "PullRequest", nil
		}
		return &gqlPRV{r: repo, p: pr}, "PullRequest", nil
	}
	return nil, "", fmt.Errorf("unknown Repository field %q", field)
}

func (e *gqlExec) resolveIssue(repo *Repo, issue *Issue, field string, args map[string]any) (any, string, error) {
	switch field {
	case "id":
		return gqlIssueID(repo, issue.Number), "", nil
	case "number":
		return issue.Number, "", nil
	case "title":
		return issue.Title, "", nil
	case "state":
		return gqlIssueState(issue.State), "", nil
	case "body":
		return issue.Body, "", nil
	case "createdAt":
		return issue.CreatedAt, "", nil
	case "updatedAt":
		return issue.UpdatedAt, "", nil
	}
	return nil, "", fmt.Errorf("unknown Issue field %q", field)
}

func (e *gqlExec) resolveIssueConn(conn *gqlIssueConnV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "nodes":
		out := make([]any, 0, len(conn.items))
		for _, issue := range conn.items {
			out = append(out, &gqlIssueV{r: conn.r, i: issue})
		}
		return out, "Issue", nil
	case "pageInfo":
		return &conn.page, "PageInfo", nil
	case "totalCount":
		return int64(conn.total), "", nil
	}
	return nil, "", fmt.Errorf("unknown IssueConnection field %q", field)
}

func (e *gqlExec) resolvePullRequest(repo *Repo, pr *PullRequest, field string, args map[string]any) (any, string, error) {
	switch field {
	case "id":
		return gqlPullID(repo, pr.Number), "", nil
	case "number":
		return pr.Number, "", nil
	case "title":
		return pr.Title, "", nil
	case "state":
		return gqlPullState(pr), "", nil
	case "body":
		return pr.Body, "", nil
	case "merged":
		return pr.Merged, "", nil
	case "headRefName":
		return pr.HeadRef, "", nil
	case "baseRefName":
		return pr.BaseRef, "", nil
	case "mergeable":
		return e.s.gqlMergeable(repo, pr), "", nil
	case "files":
		files := e.s.gqlPullFiles(repo, pr)
		if first, hasFirst := gqlIntArg(args, "first"); hasFirst && int(first) < len(files) {
			files = files[:first]
		}
		conn := &gqlFileConnV{items: files}
		conn.page.hasNext = false
		if len(files) > 0 {
			conn.page.hasStart = true
			conn.page.hasEnd = true
		}
		return conn, "PullRequestChangedFileConnection", nil
	case "createdAt":
		return pr.CreatedAt, "", nil
	case "updatedAt":
		return pr.UpdatedAt, "", nil
	case "mergedAt":
		if pr.Merged {
			return pr.MergedAt, "", nil
		}
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("unknown PullRequest field %q", field)
}

func (e *gqlExec) resolvePRConn(conn *gqlPRConnV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "nodes":
		out := make([]any, 0, len(conn.items))
		for _, pr := range conn.items {
			out = append(out, &gqlPRV{r: conn.r, p: pr})
		}
		return out, "PullRequest", nil
	case "pageInfo":
		return &conn.page, "PageInfo", nil
	case "totalCount":
		return int64(conn.total), "", nil
	}
	return nil, "", fmt.Errorf("unknown PullRequestConnection field %q", field)
}

func (e *gqlExec) resolveFileConn(conn *gqlFileConnV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "nodes":
		out := make([]any, 0, len(conn.items))
		for i := range conn.items {
			f := conn.items[i]
			out = append(out, &f)
		}
		return out, "PullRequestChangedFile", nil
	case "pageInfo":
		return &conn.page, "PageInfo", nil
	}
	return nil, "", fmt.Errorf("unknown PullRequestChangedFileConnection field %q", field)
}

func (e *gqlExec) resolveFile(f *gqlFileV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "path":
		return f.path, "", nil
	case "additions":
		return int64(f.additions), "", nil
	case "deletions":
		return int64(f.deletions), "", nil
	}
	return nil, "", fmt.Errorf("unknown PullRequestChangedFile field %q", field)
}

func (e *gqlExec) resolveUser(u *gqlUserV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "login":
		return u.login, "", nil
	case "id":
		return u.id, "", nil
	}
	return nil, "", fmt.Errorf("unknown User field %q", field)
}

func (e *gqlExec) resolvePageInfo(p *gqlPageInfoV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "hasNextPage":
		return p.hasNext, "", nil
	case "hasPreviousPage":
		return p.hasPrev, "", nil
	case "startCursor":
		if !p.hasStart {
			return nil, "", nil
		}
		return p.start, "", nil
	case "endCursor":
		if !p.hasEnd {
			return nil, "", nil
		}
		return p.end, "", nil
	}
	return nil, "", fmt.Errorf("unknown PageInfo field %q", field)
}

func (e *gqlExec) resolveComment(v *gqlCommentV, field string, args map[string]any) (any, string, error) {
	switch field {
	case "id":
		return gqlCommentID(v.r, v.i.Number, v.c.ID), "", nil
	case "body":
		return v.c.Body, "", nil
	case "createdAt":
		return v.c.CreatedAt, "", nil
	case "updatedAt":
		return v.c.UpdatedAt, "", nil
	case "author":
		return &gqlUserV{login: v.c.User, id: viewerID}, "User", nil
	}
	return nil, "", fmt.Errorf("unknown IssueComment field %q", field)
}

// -------------------------------------------------------------------------------------------
// Introspection
// -------------------------------------------------------------------------------------------

func (e *gqlExec) resolveType(t *gqlTypeV, field string, args map[string]any) (any, string, error) {
	def := gqlTypeDefOf(t.name)
	switch field {
	case "name":
		return t.name, "", nil
	case "kind":
		if def == nil {
			return nil, "", nil
		}
		return def.kind, "", nil
	case "description":
		return nil, "", nil
	case "fields":
		if def == nil {
			return nil, "", nil
		}
		out := make([]any, 0, len(def.fields))
		for _, name := range def.fields {
			out = append(out, &gqlFieldV{name: name})
		}
		return out, "__Field", nil
	case "enumValues":
		if def == nil {
			return nil, "", nil
		}
		out := make([]any, 0, len(def.enumVals))
		for _, name := range def.enumVals {
			out = append(out, &gqlEnumV{name: name})
		}
		return out, "__EnumValue", nil
	case "inputFields":
		if def == nil {
			return nil, "", nil
		}
		out := make([]any, 0, len(def.inputFields))
		for _, name := range def.inputFields {
			out = append(out, &gqlInputValV{name: name})
		}
		return out, "__InputValue", nil
	case "possibleTypes", "interfaces":
		return []any{}, "__Type", nil
	case "ofType":
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("unknown __Type field %q", field)
}

func (e *gqlExec) resolveSchema(field string, args map[string]any) (any, string, error) {
	switch field {
	case "queryType":
		return &gqlTypeV{name: "Query"}, "__Type", nil
	case "mutationType":
		return &gqlTypeV{name: "Mutation"}, "__Type", nil
	case "types":
		out := make([]any, 0, len(gqlSchema))
		for _, def := range gqlSchema {
			out = append(out, &gqlTypeV{name: def.name})
		}
		return out, "__Type", nil
	case "directives":
		return []any{}, "", nil
	}
	return nil, "", fmt.Errorf("unknown __Schema field %q", field)
}

// -------------------------------------------------------------------------------------------
// HTTP entry point: POST /api/graphql, single operation or a JSON array
// of operations (GitHub's operation batching, answered in order).
// -------------------------------------------------------------------------------------------

func (s *Store) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !s.ValidToken(r) {
		s.writeError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	runOne := func(op map[string]any) map[string]any {
		query, _ := op["query"].(string)
		vars := map[string]any{}
		if v, ok := op["variables"].(map[string]any); ok {
			vars = v
		}
		operationName, _ := op["operationName"].(string)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.gqlRunOp(query, vars, operationName)
	}
	if list, ok := raw.([]any); ok {
		out := make([]any, 0, len(list))
		for _, item := range list {
			op, _ := item.(map[string]any)
			if op == nil {
				out = append(out, map[string]any{"data": nil, "errors": []gqlErrorEntry{{Message: "batched operation is not an object"}}})
				continue
			}
			out = append(out, runOne(op))
		}
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	op, ok := raw.(map[string]any)
	if !ok {
		s.writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.writeJSON(w, http.StatusOK, runOne(op))
}
