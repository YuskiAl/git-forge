// GraphQL v4. A small parser for the documents the clients actually send
// (gh CLI and github-mcp-server shapes, plus introspection), and an executor
// that answers them from the store. Port of graphql.ts. Caller holds the
// store lock.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type gValue struct {
	kind   string // variable | string | int | float | boolean | null | enum | list | object
	str    string
	num    float64
	b      bool
	values []*gValue
	fields map[string]*gValue
}

type gSelection struct {
	alias      string
	hasAlias   bool
	name       string
	args       map[string]*gValue
	selections []*gSelection
}

type gOperation struct {
	typ       string // query | mutation
	name      string
	varDefs   map[string]*gValue
	selections []*gSelection
}

type gParser struct {
	src   string
	index int
}

func (p *gParser) peek() byte {
	if p.index < len(p.src) {
		return p.src[p.index]
	}
	return 0
}

func (p *gParser) skipWhitespace() {
	for {
		if p.index >= len(p.src) {
			return
		}
		ch := p.src[p.index]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			p.index++
			continue
		}
		if ch == '#' {
			for p.index < len(p.src) && p.src[p.index] != '\n' {
				p.index++
			}
			continue
		}
		return
	}
}

func (p *gParser) expect(ch byte) error {
	p.skipWhitespace()
	if p.index >= len(p.src) || p.src[p.index] != ch {
		return fmt.Errorf("expected '%c' at %d", ch, p.index)
	}
	p.index++
	return nil
}

func isNameChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

func (p *gParser) readName() (string, error) {
	p.skipWhitespace()
	start := p.index
	for p.index < len(p.src) && isNameChar(p.src[p.index]) {
		p.index++
	}
	if p.index == start {
		return "", fmt.Errorf("expected name at %d", p.index)
	}
	return p.src[start:p.index], nil
}

func (p *gParser) readString() (string, error) {
	p.skipWhitespace()
	if p.index < len(p.src) && p.src[p.index] == '"' {
		p.index++
		if p.index+2 < len(p.src) && p.src[p.index] == '"' && p.src[p.index+1] == '"' && p.src[p.index+2] == '"' {
			p.index += 2
			end := strings.Index(p.src[p.index:], `"""`)
			if end < 0 {
				return "", fmt.Errorf("unterminated block string")
			}
			value := p.src[p.index : p.index+end]
			p.index += end + 3
			return value, nil
		}
		var out strings.Builder
		for p.index < len(p.src) {
			ch := p.src[p.index]
			p.index++
			if ch == '"' {
				return out.String(), nil
			}
			if ch == '\\' {
				if p.index >= len(p.src) {
					break
				}
				next := p.src[p.index]
				p.index++
				switch next {
				case 'n':
					out.WriteByte('\n')
				case 't':
					out.WriteByte('\t')
				case 'r':
					out.WriteByte('\r')
				default:
					out.WriteByte(next)
				}
			} else {
				out.WriteByte(ch)
			}
		}
		return "", fmt.Errorf("unterminated string")
	}
	return p.readName()
}

func (p *gParser) readNumber() (*gValue, error) {
	p.skipWhitespace()
	start := p.index
	if p.peek() == '-' {
		p.index++
	}
	for p.index < len(p.src) && p.src[p.index] >= '0' && p.src[p.index] <= '9' {
		p.index++
	}
	isFloat := false
	if p.index < len(p.src) && p.src[p.index] == '.' {
		isFloat = true
		p.index++
		for p.index < len(p.src) && p.src[p.index] >= '0' && p.src[p.index] <= '9' {
			p.index++
		}
	}
	text := p.src[start:p.index]
	if isFloat {
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, err
		}
		return &gValue{kind: "float", num: v}, nil
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, err
	}
	return &gValue{kind: "int", num: float64(v)}, nil
}

func (p *gParser) readValue() (*gValue, error) {
	p.skipWhitespace()
	ch := p.peek()
	switch {
	case ch == '$':
		p.index++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		return &gValue{kind: "variable", str: name}, nil
	case ch == '"':
		s, err := p.readString()
		if err != nil {
			return nil, err
		}
		return &gValue{kind: "string", str: s}, nil
	case ch == '[':
		p.index++
		v := &gValue{kind: "list"}
		for {
			p.skipWhitespace()
			if p.peek() == ']' {
				p.index++
				break
			}
			item, err := p.readValue()
			if err != nil {
				return nil, err
			}
			v.values = append(v.values, item)
		}
		return v, nil
	case ch == '{':
		p.index++
		v := &gValue{kind: "object", fields: make(map[string]*gValue)}
		for {
			p.skipWhitespace()
			if p.peek() == '}' {
				p.index++
				break
			}
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			val, err := p.readValue()
			if err != nil {
				return nil, err
			}
			v.fields[name] = val
		}
		return v, nil
	case ch == '-' || ch >= '0' && ch <= '9':
		return p.readNumber()
	default:
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &gValue{kind: "boolean", b: true}, nil
		case "false":
			return &gValue{kind: "boolean", b: false}, nil
		case "null":
			return &gValue{kind: "null"}, nil
		default:
			return &gValue{kind: "enum", str: name}, nil
		}
	}
}

func (p *gParser) readArguments() (map[string]*gValue, error) {
	args := make(map[string]*gValue)
	p.skipWhitespace()
	if p.peek() != '(' {
		return args, nil
	}
	p.index++
	for {
		p.skipWhitespace()
		if p.peek() == ')' {
			p.index++
			break
		}
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		val, err := p.readValue()
		if err != nil {
			return nil, err
		}
		args[name] = val
	}
	return args, nil
}

func (p *gParser) readType() (string, error) {
	p.skipWhitespace()
	if p.peek() == '[' {
		p.index++
		inner, err := p.readType()
		if err != nil {
			return "", err
		}
		if err := p.expect(']'); err != nil {
			return "", err
		}
		t := "[" + inner + "]"
		p.skipWhitespace()
		if p.peek() == '!' {
			p.index++
			t += "!"
		}
		return t, nil
	}
	name, err := p.readName()
	if err != nil {
		return "", err
	}
	p.skipWhitespace()
	if p.peek() == '!' {
		p.index++
		return name + "!", nil
	}
	return name, nil
}

func (p *gParser) readVariableDefinitions() (map[string]*gValue, error) {
	defs := make(map[string]*gValue)
	p.skipWhitespace()
	if p.peek() != '(' {
		return defs, nil
	}
	p.index++
	for {
		p.skipWhitespace()
		if p.peek() == ')' {
			p.index++
			break
		}
		if err := p.expect('$'); err != nil {
			return nil, err
		}
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		if _, err := p.readType(); err != nil {
			return nil, err
		}
		p.skipWhitespace()
		if p.peek() == '=' {
			p.index++
			val, err := p.readValue()
			if err != nil {
				return nil, err
			}
			defs[name] = val
		} else {
			defs[name] = &gValue{kind: "null"}
		}
	}
	return defs, nil
}

func (p *gParser) readSelectionSet() ([]*gSelection, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var selections []*gSelection
	for {
		p.skipWhitespace()
		if p.peek() == '}' {
			p.index++
			break
		}
		if p.peek() == '.' && p.index+2 < len(p.src) && p.src[p.index+1] == '.' && p.src[p.index+2] == '.' {
			p.index += 3
			p.skipWhitespace()
			var typeCondition string
			if p.index+2 <= len(p.src) && p.src[p.index:p.index+2] == "on" {
				p.index += 2
				name, err := p.readName()
				if err != nil {
					return nil, err
				}
				typeCondition = name
			}
			nested, err := p.readSelectionSet()
			if err != nil {
				return nil, err
			}
			selections = append(selections, &gSelection{name: "...on " + typeCondition, args: make(map[string]*gValue), selections: nested})
			continue
		}
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		alias := ""
		hasAlias := false
		p.skipWhitespace()
		if p.peek() == ':' {
			p.index++
			alias = name
			hasAlias = true
			name, err = p.readName()
			if err != nil {
				return nil, err
			}
		}
		args, err := p.readArguments()
		if err != nil {
			return nil, err
		}
		p.skipWhitespace()
		var nested []*gSelection
		if p.peek() == '{' {
			nested, err = p.readSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		selections = append(selections, &gSelection{alias: alias, hasAlias: hasAlias, name: name, args: args, selections: nested})
	}
	return selections, nil
}

func (p *gParser) readDocument() ([]*gOperation, error) {
	var operations []*gOperation
	for {
		p.skipWhitespace()
		if p.index >= len(p.src) {
			break
		}
		if p.peek() == '{' {
			sels, err := p.readSelectionSet()
			if err != nil {
				return nil, err
			}
			operations = append(operations, &gOperation{typ: "query", varDefs: make(map[string]*gValue), selections: sels})
			continue
		}
		keyword, err := p.readName()
		if err != nil {
			return nil, err
		}
		if keyword != "query" && keyword != "mutation" {
			return nil, fmt.Errorf("unsupported document keyword %s", keyword)
		}
		typ := keyword
		p.skipWhitespace()
		var name string
		if p.index < len(p.src) {
			c := p.src[p.index]
			if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' {
				name, err = p.readName()
				if err != nil {
					return nil, err
				}
			}
		}
		varDefs, err := p.readVariableDefinitions()
		if err != nil {
			return nil, err
		}
		sels, err := p.readSelectionSet()
		if err != nil {
			return nil, err
		}
		operations = append(operations, &gOperation{typ: typ, name: name, varDefs: varDefs, selections: sels})
	}
	return operations, nil
}

// ---------------------------------------------------------------------------
// values
// ---------------------------------------------------------------------------

func resolveGValue(v *gValue, variables map[string]interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "variable":
		if val, ok := variables[v.str]; ok {
			return val
		}
		return nil
	case "string":
		return v.str
	case "int", "float":
		return v.num
	case "boolean":
		return v.b
	case "null":
		return nil
	case "enum":
		return v.str
	case "list":
		out := make([]interface{}, 0, len(v.values))
		for _, item := range v.values {
			out = append(out, resolveGValue(item, variables))
		}
		return out
	case "object":
		out := make(map[string]interface{})
		for k, item := range v.fields {
			out[k] = resolveGValue(item, variables)
		}
		return out
	}
	return nil
}

func gArg(sel *gSelection, name string, variables map[string]interface{}) interface{} {
	v, ok := sel.args[name]
	if !ok {
		return nil
	}
	return resolveGValue(v, variables)
}

func gArgNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// ids and field sets
// ---------------------------------------------------------------------------

func encodeGlobalID(kind string, parts ...interface{}) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteByte(':')
		sb.WriteString(fmt.Sprintf("%v", p))
	}
	return base64.StdEncoding.EncodeToString([]byte(sb.String()))
}

func decodeGlobalID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	segs := strings.Split(string(raw), ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

var fieldSets = map[string][]string{
	"Query":                         {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation":                      {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                          {"login", "id"},
	"Repository":                    {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"Ref":                           {"name", "target"},
	"Issue":                         {"id", "number", "title", "state", "body"},
	"IssueConnection":               {"nodes", "edges", "totalCount", "pageInfo"},
	"IssueEdge":                     {"node", "cursor"},
	"PageInfo":                      {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"PullRequest":                   {"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"},
	"PullRequestConnection":         {"nodes", "edges", "totalCount", "pageInfo"},
	"PullRequestEdge":               {"node", "cursor"},
	"PullRequestChangedFile":        {"path"},
	"PullRequestChangedFileConnection": {"nodes", "pageInfo"},
	"CreateIssuePayload":            {"issue", "clientMutationId"},
	"CloseIssuePayload":             {"issue", "clientMutationId"},
	"CreatePullRequestPayload":      {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":       {"pullRequest", "clientMutationId"},
	"AddCommentPayload":             {"commentEdge", "clientMutationId"},
	"IssueCommentEdge":              {"node", "cursor"},
	"IssueComment":                  {"id", "body"},
	"__Type":                        {"kind", "name", "description", "fields", "enumValues", "inputFields", "interfaces", "possibleTypes"},
	"__Field":                       {"name", "description", "args", "type"},
	"__EnumValue":                   {"name", "description"},
	"__InputValue":                  {"name", "type"},
}

var enumValues = map[string][]string{
	"SearchType":       {"ISSUE", "REPOSITORY", "USER"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":   {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"MergeMethod":      {"MERGE", "SQUASH", "REBASE"},
	"IssueOrderField":  {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":   {"ASC", "DESC"},
}

func fieldAllowed(typeName, field string) bool {
	for _, f := range fieldSets[typeName] {
		if f == field {
			return true
		}
	}
	return false
}

func cursorFor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

func numberFromCursor(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	v, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type gCtx struct {
	store     *Store
	variables map[string]interface{}
	errors    []map[string]interface{}
}

func (c *gCtx) fieldError(typeName, field string) interface{} {
	c.errors = append(c.errors, map[string]interface{}{"message": fmt.Sprintf("Cannot query field %q on type %q.", field, typeName)})
	return nil
}

type gResolver func(ctx *gCtx, typeName string, source interface{}, sel *gSelection) interface{}

func (c *gCtx) resolveSelections(typeName string, source interface{}, selections []*gSelection, resolver gResolver) map[string]interface{} {
	if source == nil {
		return nil
	}
	if selections == nil {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{})
	for _, sel := range selections {
		key := sel.name
		if sel.hasAlias {
			key = sel.alias
		}
		if strings.HasPrefix(sel.name, "...on ") {
			wanted := strings.TrimSpace(strings.TrimPrefix(sel.name, "...on "))
			if wanted == "" || wanted == typeName {
				nested := c.resolveSelections(typeName, source, sel.selections, resolver)
				for k, v := range nested {
					out[k] = v
				}
			}
			continue
		}
		if !fieldAllowed(typeName, sel.name) {
			out[key] = c.fieldError(typeName, sel.name)
			continue
		}
		out[key] = resolver(c, typeName, source, sel)
	}
	return out
}

func issueStateG(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func prStateG(pull *PullRequest) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func (c *gCtx) resolveIssue(repoID int, issue *Issue, sel *gSelection) interface{} {
	switch sel.name {
	case "id":
		return encodeGlobalID("Issue", repoID, issue.Number)
	case "number":
		return issue.Number
	case "title":
		return issue.Title
	case "state":
		return issueStateG(issue)
	case "body":
		return issue.Body
	default:
		return c.fieldError("Issue", sel.name)
	}
}

func (c *gCtx) resolvePull(repo *Repo, pull *PullRequest, sel *gSelection) interface{} {
	switch sel.name {
	case "id":
		return encodeGlobalID("PullRequest", repo.ID, pull.Number)
	case "number":
		return pull.Number
	case "title":
		return pull.Title
	case "state":
		return prStateG(pull)
	case "merged":
		return pull.Merged
	case "headRefName":
		return pull.Head
	case "baseRefName":
		return pull.Base
	case "mergeable":
		if prConflicts(repo, pull) {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "files":
		base := mergeBase(repo, pull.HeadSha, pull.BaseSha)
		baseFiles := make(map[string]FlatFile)
		if base != "" {
			baseFiles = filesAt(repo, base)
		}
		headFiles := filesAt(repo, pull.HeadSha)
		var paths []map[string]interface{}
		for _, ch := range diffFlat(baseFiles, headFiles) {
			paths = append(paths, map[string]interface{}{"path": ch.Path})
		}
		if first, ok := gArgNumber(gArg(sel, "first", c.variables)); ok {
			n := int(first)
			if n < len(paths) {
				paths = paths[:n]
			}
		}
		conn := map[string]interface{}{"nodes": paths, "hasNext": false}
		return c.resolveSelections("PullRequestChangedFileConnection", conn, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			connection := source.(map[string]interface{})
			nodes := connection["nodes"].([]map[string]interface{})
			switch s.name {
			case "nodes":
				out := make([]interface{}, 0, len(nodes))
				for _, node := range nodes {
					out = append(out, inner.resolveSelections("PullRequestChangedFile", node, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
						if s2.name == "path" {
							return src.(map[string]interface{})["path"]
						}
						return c2.fieldError("PullRequestChangedFile", s2.name)
					}))
				}
				return out
			case "pageInfo":
				return inner.resolveSelections("PageInfo", map[string]interface{}{}, s.selections, func(c2 *gCtx, _ string, _ interface{}, s2 *gSelection) interface{} {
					switch s2.name {
					case "hasNextPage":
						return connection["hasNext"]
					case "endCursor":
						return nil
					case "hasPreviousPage":
						return false
					case "startCursor":
						return nil
					default:
						return c2.fieldError("PageInfo", s2.name)
					}
				})
			default:
				return inner.fieldError("PullRequestChangedFileConnection", s.name)
			}
		})
	default:
		return c.fieldError("PullRequest", sel.name)
	}
}

func (c *gCtx) resolveRepository(repo *Repo, sel *gSelection) interface{} {
	switch sel.name {
	case "id":
		return encodeGlobalID("Repository", repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		return c.resolveSelections("User", map[string]interface{}{"login": repo.Owner}, sel.selections, func(inner *gCtx, _ string, _ interface{}, s *gSelection) interface{} {
			switch s.name {
			case "login":
				return repo.Owner
			case "id":
				return encodeGlobalID("User", repo.Owner)
			default:
				return inner.fieldError("User", s.name)
			}
		})
	case "defaultBranchRef":
		tip, ok := headTarget(repo)
		if !ok {
			return nil
		}
		_ = tip
		return c.resolveSelections("Ref", map[string]interface{}{"name": repo.DefaultBranch}, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			switch s.name {
			case "name":
				return source.(map[string]interface{})["name"]
			case "target":
				return nil
			default:
				return inner.fieldError("Ref", s.name)
			}
		})
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		number, ok := gArgNumber(gArg(sel, "number", c.variables))
		if !ok {
			return nil
		}
		issue := repo.Issues[int(number)]
		if issue == nil {
			return nil
		}
		return c.resolveSelections("Issue", issue, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			return inner.resolveIssue(repo.ID, source.(*Issue), s)
		})
	case "issues":
		first, hasFirst := gArgNumber(gArg(sel, "first", c.variables))
		states := gArg(sel, "states", c.variables)
		orderBy := gArg(sel, "orderBy", c.variables)
		after := gArg(sel, "after", c.variables)
		var list []*Issue
		for _, iss := range repo.IssueOrder {
			list = append(list, iss)
		}
		if arr, ok := states.([]interface{}); ok && len(arr) > 0 {
			wanted := make(map[string]bool)
			for _, st := range arr {
				wanted[fmt.Sprintf("%v", st)] = true
			}
			var filtered []*Issue
			for _, iss := range list {
				if wanted[issueStateG(iss)] {
					filtered = append(filtered, iss)
				}
			}
			list = filtered
		}
		direction := -1
		if m, ok := orderBy.(map[string]interface{}); ok {
			if d, ok := m["direction"].(string); ok && d == "ASC" {
				direction = 1
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if direction == 1 {
				return list[i].Number < list[j].Number
			}
			return list[i].Number > list[j].Number
		})
		if afterStr, ok := after.(string); ok {
			if afterNumber, ok := numberFromCursor(afterStr); ok {
				var filtered []*Issue
				for _, iss := range list {
					if direction == 1 && iss.Number > afterNumber || direction == -1 && iss.Number < afterNumber {
						filtered = append(filtered, iss)
					}
				}
				list = filtered
			}
		}
		limit := len(list)
		if hasFirst {
			limit = int(first)
		}
		var nodes []*Issue
		hasNext := false
		if limit < len(list) {
			nodes = list[:limit]
			hasNext = true
		} else {
			nodes = list
		}
		var endCursor interface{}
		if len(nodes) > 0 {
			endCursor = cursorFor(nodes[len(nodes)-1].Number)
		}
		conn := map[string]interface{}{"nodes": nodes, "hasNext": hasNext, "endCursor": endCursor}
		return c.resolveSelections("IssueConnection", conn, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			connection := source.(map[string]interface{})
			nodes := connection["nodes"].([]*Issue)
			switch s.name {
			case "nodes":
				out := make([]interface{}, 0, len(nodes))
				for _, iss := range nodes {
					out = append(out, inner.resolveSelections("Issue", iss, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
						return c2.resolveIssue(repo.ID, src.(*Issue), s2)
					}))
				}
				return out
			case "edges":
				out := make([]interface{}, 0, len(nodes))
				for _, iss := range nodes {
					edge := map[string]interface{}{"node": iss, "cursor": cursorFor(iss.Number)}
					out = append(out, inner.resolveSelections("IssueEdge", edge, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
						e := src.(map[string]interface{})
						switch s2.name {
						case "node":
							return c2.resolveSelections("Issue", e["node"].(*Issue), s2.selections, func(c3 *gCtx, _ string, src3 interface{}, s3 *gSelection) interface{} {
								return c3.resolveIssue(repo.ID, src3.(*Issue), s3)
							})
						case "cursor":
							return e["cursor"]
						default:
							return c2.fieldError("IssueEdge", s2.name)
						}
					}))
				}
				return out
			case "totalCount":
				return len(repo.Issues)
			case "pageInfo":
				return inner.resolveSelections("PageInfo", map[string]interface{}{}, s.selections, func(c2 *gCtx, _ string, _ interface{}, s2 *gSelection) interface{} {
					switch s2.name {
					case "hasNextPage":
						return connection["hasNext"]
					case "endCursor":
						return connection["endCursor"]
					case "hasPreviousPage":
						return false
					case "startCursor":
						return nil
					default:
						return c2.fieldError("PageInfo", s2.name)
					}
				})
			default:
				return inner.fieldError("IssueConnection", s.name)
			}
		})
	case "pullRequests":
		first, hasFirst := gArgNumber(gArg(sel, "first", c.variables))
		var list []*PullRequest
		for _, p := range repo.PullOrder {
			list = append(list, p)
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Number > list[j].Number })
		limit := len(list)
		if hasFirst {
			limit = int(first)
		}
		var nodes []*PullRequest
		if limit < len(list) {
			nodes = list[:limit]
		} else {
			nodes = list
		}
		conn := map[string]interface{}{"nodes": nodes}
		return c.resolveSelections("PullRequestConnection", conn, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			connection := source.(map[string]interface{})
			nodes := connection["nodes"].([]*PullRequest)
			switch s.name {
			case "nodes":
				out := make([]interface{}, 0, len(nodes))
				for _, pull := range nodes {
					out = append(out, inner.resolveSelections("PullRequest", pull, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
						return c2.resolvePull(repo, src.(*PullRequest), s2)
					}))
				}
				return out
			case "edges":
				return []interface{}{}
			case "totalCount":
				return len(repo.Pulls)
			case "pageInfo":
				return inner.resolveSelections("PageInfo", map[string]interface{}{}, s.selections, func(c2 *gCtx, _ string, _ interface{}, s2 *gSelection) interface{} {
					switch s2.name {
					case "hasNextPage":
						return false
					case "endCursor":
						return nil
					case "hasPreviousPage":
						return false
					case "startCursor":
						return nil
					default:
						return c2.fieldError("PageInfo", s2.name)
					}
				})
			default:
				return inner.fieldError("PullRequestConnection", s.name)
			}
		})
	case "pullRequest":
		number, ok := gArgNumber(gArg(sel, "number", c.variables))
		if !ok {
			return nil
		}
		pull := repo.Pulls[int(number)]
		if pull == nil {
			return nil
		}
		return c.resolveSelections("PullRequest", pull, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			return inner.resolvePull(repo, source.(*PullRequest), s)
		})
	default:
		return c.fieldError("Repository", sel.name)
	}
}

func (c *gCtx) resolveRootField(op *gOperation, sel *gSelection) interface{} {
	if op.typ == "query" {
		switch sel.name {
		case "viewer":
			return c.resolveSelections("User", map[string]interface{}{"login": viewerLogin}, sel.selections, func(inner *gCtx, _ string, _ interface{}, s *gSelection) interface{} {
				switch s.name {
				case "login":
					return viewerLogin
				case "id":
					return encodeGlobalID("User", viewerLogin)
				default:
					return inner.fieldError("User", s.name)
				}
			})
		case "repository":
			owner, _ := gArg(sel, "owner", c.variables).(string)
			name, _ := gArg(sel, "name", c.variables).(string)
			if owner == "" || name == "" {
				// Node: typeof owner !== 'string' → null. Empty string here
				// means missing/wrong type (a real empty name is also null
				// in practice since no repo has an empty name).
				if _, ok := gArg(sel, "owner", c.variables).(string); !ok {
					return nil
				}
				if _, ok := gArg(sel, "name", c.variables).(string); !ok {
					return nil
				}
			}
			repo := c.store.getRepoLocked(owner, name)
			if repo == nil {
				return nil
			}
			return c.resolveSelections("Repository", repo, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
				return inner.resolveRepository(source.(*Repo), s)
			})
		case "node":
			id, _ := gArg(sel, "id", c.variables).(string)
			if _, ok := gArg(sel, "id", c.variables).(string); !ok {
				return nil
			}
			kind, parts, ok := decodeGlobalID(id)
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				repoID, err := strconv.Atoi(parts[0])
				if err != nil {
					return nil
				}
				repo := c.store.repoByIDLocked(repoID)
				if repo == nil {
					return nil
				}
				return c.resolveSelections("Repository", repo, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
					return inner.resolveRepository(source.(*Repo), s)
				})
			case "Issue":
				repoID, err1 := strconv.Atoi(parts[0])
				number, err2 := strconv.Atoi(parts[1])
				if err1 != nil || err2 != nil {
					return nil
				}
				repo := c.store.repoByIDLocked(repoID)
				if repo == nil {
					return nil
				}
				issue := repo.Issues[number]
				if issue == nil {
					return nil
				}
				return c.resolveSelections("Issue", issue, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
					return inner.resolveIssue(repo.ID, source.(*Issue), s)
				})
			case "PullRequest":
				repoID, err1 := strconv.Atoi(parts[0])
				number, err2 := strconv.Atoi(parts[1])
				if err1 != nil || err2 != nil {
					return nil
				}
				repo := c.store.repoByIDLocked(repoID)
				if repo == nil {
					return nil
				}
				pull := repo.Pulls[number]
				if pull == nil {
					return nil
				}
				return c.resolveSelections("PullRequest", pull, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
					return inner.resolvePull(repo, source.(*PullRequest), s)
				})
			default:
				return nil
			}
		case "__type":
			name, _ := gArg(sel, "name", c.variables).(string)
			if _, ok := gArg(sel, "name", c.variables).(string); !ok {
				return nil
			}
			if _, ok := fieldSets[name]; !ok {
				if _, ok := enumValues[name]; !ok {
					return nil
				}
			}
			isEnum := false
			if _, ok := enumValues[name]; ok {
				isEnum = true
			}
			typeSource := map[string]interface{}{"name": name, "isEnum": isEnum}
			return c.resolveSelections("__Type", typeSource, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
				typed := source.(map[string]interface{})
				switch s.name {
				case "name":
					return typed["name"]
				case "kind":
					if typed["isEnum"].(bool) {
						return "ENUM"
					}
					return "OBJECT"
				case "description":
					return nil
				case "fields":
					fields := fieldSets[typed["name"].(string)]
					out := make([]interface{}, 0, len(fields))
					for _, fieldName := range fields {
						out = append(out, inner.resolveSelections("__Field", map[string]interface{}{"name": fieldName}, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
							switch s2.name {
							case "name":
								return src.(map[string]interface{})["name"]
							case "description":
								return nil
							case "args":
								return []interface{}{}
							case "type":
								return nil
							default:
								return c2.fieldError("__Field", s2.name)
							}
						}))
					}
					return out
				case "enumValues":
					values := enumValues[typed["name"].(string)]
					out := make([]interface{}, 0, len(values))
					for _, valueName := range values {
						out = append(out, inner.resolveSelections("__EnumValue", map[string]interface{}{"name": valueName}, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
							switch s2.name {
							case "name":
								return src.(map[string]interface{})["name"]
							case "description":
								return nil
							default:
								return c2.fieldError("__EnumValue", s2.name)
							}
						}))
					}
					return out
				case "inputFields":
					return []interface{}{}
				case "interfaces":
					return []interface{}{}
				case "possibleTypes":
					return []interface{}{}
				default:
					return inner.fieldError("__Type", s.name)
				}
			})
		case "__schema":
			return c.resolveSelections("__Type", map[string]interface{}{"name": "Query", "isEnum": false}, sel.selections, func(inner *gCtx, _ string, _ interface{}, _ *gSelection) interface{} {
				return nil
			})
		default:
			return c.fieldError("Query", sel.name)
		}
	}

	// Mutations.
	switch sel.name {
	case "createIssue":
		input, _ := gArg(sel, "input", c.variables).(map[string]interface{})
		if input == nil {
			return nil
		}
		repositoryID, _ := input["repositoryId"].(string)
		kind, parts, ok := decodeGlobalID(repositoryID)
		if !ok || kind != "Repository" {
			return nil
		}
		repoID, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		repo := c.store.repoByIDLocked(repoID)
		if repo == nil {
			return nil
		}
		number := repo.NextNumber
		repo.NextNumber++
		seq := c.store.tickLocked()
		title, _ := input["title"].(string)
		bodyStr, _ := input["body"].(string)
		issue := &Issue{
			ID: number, Number: number, Title: title, Body: bodyStr,
			State: "open", Labels: []string{}, User: viewerLogin,
			CreatedSeq: seq, UpdatedSeq: seq,
		}
		repo.Issues[number] = issue
		repo.IssueOrder = append(repo.IssueOrder, issue)
		return c.resolveSelections("CreateIssuePayload", map[string]interface{}{"issue": issue}, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			switch s.name {
			case "issue":
				return inner.resolveSelections("Issue", source.(map[string]interface{})["issue"], s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
					return c2.resolveIssue(repo.ID, src.(*Issue), s2)
				})
			case "clientMutationId":
				return nil
			default:
				return inner.fieldError("CreateIssuePayload", s.name)
			}
		})
	case "closeIssue":
		input, _ := gArg(sel, "input", c.variables).(map[string]interface{})
		if input == nil {
			return nil
		}
		issueID, _ := input["issueId"].(string)
		kind, parts, ok := decodeGlobalID(issueID)
		if !ok || kind != "Issue" {
			return nil
		}
		repoID, err1 := strconv.Atoi(parts[0])
		number, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		repo := c.store.repoByIDLocked(repoID)
		if repo == nil {
			return nil
		}
		issue := repo.Issues[number]
		if issue == nil {
			return nil
		}
		issue.State = "closed"
		issue.UpdatedSeq = c.store.tickLocked()
		clearIssueCache(issue)
		return c.resolveSelections("CloseIssuePayload", map[string]interface{}{"issue": issue}, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			switch s.name {
			case "issue":
				return inner.resolveSelections("Issue", source.(map[string]interface{})["issue"], s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
					return c2.resolveIssue(repo.ID, src.(*Issue), s2)
				})
			case "clientMutationId":
				return nil
			default:
				return inner.fieldError("CloseIssuePayload", s.name)
			}
		})
	case "createPullRequest":
		input, _ := gArg(sel, "input", c.variables).(map[string]interface{})
		if input == nil {
			return nil
		}
		repositoryID, _ := input["repositoryId"].(string)
		kind, parts, ok := decodeGlobalID(repositoryID)
		if !ok || kind != "Repository" {
			return nil
		}
		repoID, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		repo := c.store.repoByIDLocked(repoID)
		if repo == nil {
			return nil
		}
		base, _ := input["baseRefName"].(string)
		head, _ := input["headRefName"].(string)
		headSha, okH := repo.Refs["refs/heads/"+head]
		baseSha, okB := repo.Refs["refs/heads/"+base]
		if !okH || !okB {
			return nil
		}
		number := repo.NextNumber
		repo.NextNumber++
		seq := c.store.tickLocked()
		title, _ := input["title"].(string)
		bodyStr, _ := input["body"].(string)
		pull := &PullRequest{
			ID: number, Number: number, Title: title, Body: bodyStr,
			State: "open", Head: head, Base: base,
			HeadSha: headSha, BaseSha: baseSha,
			User: viewerLogin, CreatedSeq: seq, UpdatedSeq: seq,
		}
		repo.Pulls[number] = pull
		repo.PullOrder = append(repo.PullOrder, pull)
		return c.resolveSelections("CreatePullRequestPayload", map[string]interface{}{"pull": pull}, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			switch s.name {
			case "pullRequest":
				return inner.resolveSelections("PullRequest", source.(map[string]interface{})["pull"], s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
					return c2.resolvePull(repo, src.(*PullRequest), s2)
				})
			case "clientMutationId":
				return nil
			default:
				return inner.fieldError("CreatePullRequestPayload", s.name)
			}
		})
	case "mergePullRequest":
		input, _ := gArg(sel, "input", c.variables).(map[string]interface{})
		if input == nil {
			return nil
		}
		prID, _ := input["pullRequestId"].(string)
		kind, parts, ok := decodeGlobalID(prID)
		if !ok || kind != "PullRequest" {
			return nil
		}
		repoID, err1 := strconv.Atoi(parts[0])
		number, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		repo := c.store.repoByIDLocked(repoID)
		if repo == nil {
			return nil
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return nil
		}
		mergePayload := func() interface{} {
			return c.resolveSelections("MergePullRequestPayload", map[string]interface{}{"pull": pull}, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
				switch s.name {
				case "pullRequest":
					return inner.resolveSelections("PullRequest", source.(map[string]interface{})["pull"], s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
						return c2.resolvePull(repo, src.(*PullRequest), s2)
					})
				case "clientMutationId":
					return nil
				default:
					return inner.fieldError("MergePullRequestPayload", s.name)
				}
			})
		}
		if pull.State != "open" || prConflicts(repo, pull) {
			return mergePayload()
		}
		mergeMethod, _ := input["mergeMethod"].(string)
		if mergeMethod == "" {
			mergeMethod = "MERGE"
		}
		baseTipSha := pull.BaseSha
		if v, ok := repo.Refs["refs/heads/"+pull.Base]; ok {
			baseTipSha = v
		}
		base := mergeBase(repo, pull.HeadSha, baseTipSha)
		baseFiles := make(map[string]FlatFile)
		if base != "" {
			baseFiles = filesAt(repo, base)
		}
		headFiles := filesAt(repo, pull.HeadSha)
		baseTipFiles := filesAt(repo, baseTipSha)
		merged := mergedFiles(baseTipFiles, baseFiles, headFiles)
		tree := buildTreeFromFlat(repo, merged)
		var parents []string
		var message string
		if mergeMethod == "SQUASH" {
			parents = []string{baseTipSha}
			message = strings.TrimSpace(pull.Title + " (#" + strconv.Itoa(pull.Number) + ")")
		} else {
			parents = []string{baseTipSha, pull.HeadSha}
			message = "Merge pull request #" + strconv.Itoa(pull.Number) + " from " + repo.Owner + "/" + pull.Head + "\n\n" + pull.Title
		}
		commit := makeServerCommit(c.store, repo, tree, parents, message)
		repo.Refs["refs/heads/"+pull.Base] = commit
		pull.State = "closed"
		pull.Merged = true
		pull.MergedAt = isoStr(epochSec + c.store.seq)
		pull.MergeCommitSha = commit
		pull.UpdatedSeq = c.store.tickLocked()
		return mergePayload()
	case "addComment":
		input, _ := gArg(sel, "input", c.variables).(map[string]interface{})
		if input == nil {
			return nil
		}
		subjectID, _ := input["subjectId"].(string)
		kind, parts, ok := decodeGlobalID(subjectID)
		if !ok || kind != "Issue" {
			return nil
		}
		repoID, err1 := strconv.Atoi(parts[0])
		number, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		repo := c.store.repoByIDLocked(repoID)
		if repo == nil {
			return nil
		}
		issue := repo.Issues[number]
		if issue == nil {
			return nil
		}
		commentID := repo.NextCommentID
		repo.NextCommentID++
		bodyStr, _ := input["body"].(string)
		comment := &IssueComment{ID: commentID, Body: bodyStr, User: viewerLogin, CreatedSeq: c.store.tickLocked()}
		issue.Comments = append(issue.Comments, comment)
		clearIssueCache(issue)
		commentNode := map[string]interface{}{"id": encodeGlobalID("IssueComment", repo.ID, number, commentID), "body": comment.Body}
		return c.resolveSelections("AddCommentPayload", map[string]interface{}{"comment": commentNode}, sel.selections, func(inner *gCtx, _ string, source interface{}, s *gSelection) interface{} {
			switch s.name {
			case "commentEdge":
				return inner.resolveSelections("IssueCommentEdge", map[string]interface{}{"node": source.(map[string]interface{})["comment"]}, s.selections, func(c2 *gCtx, _ string, src interface{}, s2 *gSelection) interface{} {
					edge := src.(map[string]interface{})
					switch s2.name {
					case "node":
						return c2.resolveSelections("IssueComment", edge["node"], s2.selections, func(c3 *gCtx, _ string, src3 interface{}, s3 *gSelection) interface{} {
							switch s3.name {
							case "id":
								return src3.(map[string]interface{})["id"]
							case "body":
								return src3.(map[string]interface{})["body"]
							default:
								return c3.fieldError("IssueComment", s3.name)
							}
						})
					case "cursor":
						return nil
					default:
						return c2.fieldError("IssueCommentEdge", s2.name)
					}
				})
			case "clientMutationId":
				return nil
			default:
				return inner.fieldError("AddCommentPayload", s.name)
			}
		})
	default:
		return c.fieldError("Mutation", sel.name)
	}
}

func (c *gCtx) executeOperation(op *gOperation) map[string]interface{} {
	data := make(map[string]interface{})
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	for _, sel := range op.selections {
		key := sel.name
		if sel.hasAlias {
			key = sel.alias
		}
		if strings.HasPrefix(sel.name, "...") {
			continue
		}
		if !fieldAllowed(rootType, sel.name) {
			data[key] = c.fieldError(rootType, sel.name)
			continue
		}
		data[key] = c.resolveRootField(op, sel)
	}
	result := map[string]interface{}{"data": data}
	if len(c.errors) > 0 {
		errs := make([]interface{}, 0, len(c.errors))
		for _, e := range c.errors {
			errs = append(errs, e)
		}
		result["errors"] = errs
	}
	return result
}

// handleGraphQLLocked executes a GraphQL request body (already JSON-parsed).
// Returns the Result to send. Caller holds the store lock.
func handleGraphQLLocked(store *Store, requestID string, body interface{}) Result {
	if body == nil {
		return errorResult(400, "request body must be a JSON object or array", requestID)
	}
	documents, isArray := body.([]interface{})
	if !isArray {
		if _, isObj := body.(map[string]interface{}); !isObj {
			return errorResult(400, "request body must be a JSON object or array", requestID)
		}
		documents = []interface{}{body}
	}
	var results []interface{} = []interface{}{}
	for _, doc := range documents {
		docMap, ok := doc.(map[string]interface{})
		if !ok {
			results = append(results, map[string]interface{}{
				"data":   map[string]interface{}{},
				"errors": []interface{}{map[string]interface{}{"message": "request body must be a JSON object"}},
			})
			continue
		}
		query, ok := docMap["query"].(string)
		if !ok {
			results = append(results, map[string]interface{}{
				"data":   map[string]interface{}{},
				"errors": []interface{}{map[string]interface{}{"message": "query must be a string"}},
			})
			continue
		}
		variables := map[string]interface{}{}
		if v, ok := docMap["variables"].(map[string]interface{}); ok {
			variables = v
		}
		operations, err := (&gParser{src: query}).readDocument()
		if err != nil {
			results = append(results, map[string]interface{}{
				"data":   map[string]interface{}{},
				"errors": []interface{}{map[string]interface{}{"message": err.Error()}},
			})
			continue
		}
		if len(operations) == 0 {
			results = append(results, map[string]interface{}{
				"data":   map[string]interface{}{},
				"errors": []interface{}{map[string]interface{}{"message": "no operation found"}},
			})
			continue
		}
		ctx := &gCtx{store: store, variables: variables}
		results = append(results, ctx.executeOperation(operations[0]))
	}
	var payload interface{}
	if isArray {
		payload = results
	} else {
		payload = results[0]
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	return jsonResult(200, bytes.TrimRight(buf.Bytes(), "\n"), requestID)
}
