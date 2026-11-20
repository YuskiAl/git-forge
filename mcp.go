// MCP (Model Context Protocol), Streamable HTTP at /mcp. Port of mcp.ts.
// Caller holds the store lock.
package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-03-26", "2024-11-05"}
const mcpServerName = "git-forge"
const mcpServerVersion = "1.0.0"

type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"inputSchema"`
}

func mcpTools() []mcpToolDef {
	mk := func(name, desc string, props map[string]string, required []string) mcpToolDef {
		var t mcpToolDef
		t.Name = name
		t.Description = desc
		t.InputSchema.Type = "object"
		t.InputSchema.Properties = make(map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		})
		for k, v := range props {
			parts := strings.SplitN(v, "|", 2)
			t.InputSchema.Properties[k] = struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}{Type: parts[0], Description: parts[1]}
		}
		t.InputSchema.Required = required
		return t
	}
	return []mcpToolDef{
		mk("create_repo", "Create a new repository owned by the authenticated user.",
			map[string]string{"name": "string|The repository name.", "description": "string|A short description of the repository.", "private": "boolean|Whether the repository is private."},
			[]string{"name"}),
		mk("get_repo", "Get a repository by owner and name.",
			map[string]string{"owner": "string|The repository owner login.", "repo": "string|The repository name."},
			[]string{"owner", "repo"}),
		mk("create_issue", "Create an issue in a repository.",
			map[string]string{"owner": "string|The repository owner login.", "repo": "string|The repository name.", "title": "string|The issue title.", "body": "string|The issue body."},
			[]string{"owner", "repo", "title"}),
		mk("list_issues", "List issues in a repository.",
			map[string]string{"owner": "string|The repository owner login.", "repo": "string|The repository name.", "state": "string|Filter by state: open, closed, or all."},
			[]string{"owner", "repo"}),
		mk("create_pull_request", "Create a pull request from a head branch into a base branch.",
			map[string]string{"owner": "string|The repository owner login.", "repo": "string|The repository name.", "title": "string|The pull request title.", "body": "string|The pull request body.", "head": "string|The head branch name.", "base": "string|The base branch name."},
			[]string{"owner", "repo", "title", "head", "base"}),
		mk("merge_pull_request", "Merge an open pull request by number.",
			map[string]string{"owner": "string|The repository owner login.", "repo": "string|The repository name.", "pull_number": "integer|The pull request number."},
			[]string{"owner", "repo", "pull_number"}),
	}
}

func mcpJSON(status int, value interface{}) Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	body := bytes.TrimRight(buf.Bytes(), "\n")
	return Result{Status: status, Body: body, Extra: [][2]string{{"Content-Type", "application/json; charset=utf-8"}}}
}

func mcpRPCError(id interface{}, code int, message string) Result {
	return mcpJSON(200, map[string]interface{}{"jsonrpc": "2.0", "id": id, "error": map[string]interface{}{"code": code, "message": message}})
}

func mcpRPCResult(id interface{}, result interface{}) Result {
	return mcpJSON(200, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
}

func mcpToolText(payload interface{}) map[string]interface{} {
	text, _ := marshalNoEscape(payload)
	return map[string]interface{}{
		"content":           []interface{}{map[string]interface{}{"type": "text", "text": string(text)}},
		"structuredContent": payload,
	}
}

func mcpToolError(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": message}},
		"isError": true,
	}
}

func marshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n"), err
}

type mcpCtx struct {
	store     *Store
	requestID string
	host      string
}

// rest calls the REST layer in-process, reusing its validation and shapes.
func (ctx *mcpCtx) rest(method, path string, body map[string]interface{}) (int, interface{}) {
	u, _ := url.Parse(path)
	segments := []string{}
	for _, part := range strings.Split(u.Path, "/") {
		if part != "" {
			segments = append(segments, part)
		}
	}
	apiSegments := segments
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		apiSegments = segments[2:]
	}
	result := handleRestLocked(RestCtx{Store: ctx.store, RequestID: ctx.requestID, Host: ctx.host}, method, apiSegments, u.Query(), body)
	var value interface{} = string(result.Body)
	if len(result.Body) > 0 {
		var parsed interface{}
		if err := json.Unmarshal(result.Body, &parsed); err == nil {
			value = parsed
		}
	}
	return result.Status, value
}

func mcpPickArg(args map[string]interface{}, names ...string) interface{} {
	for _, name := range names {
		if v, ok := args[name]; ok {
			return v
		}
	}
	return nil
}

func mcpCallTool(ctx *mcpCtx, name string, args map[string]interface{}) interface{} {
	owner, _ := mcpPickArg(args, "owner").(string)
	repoName, _ := mcpPickArg(args, "repo", "repository").(string)
	fullName, _ := mcpPickArg(args, "full_name").(string)
	var ownerFromFull, repoFromFull string
	if strings.Contains(fullName, "/") {
		parts := strings.SplitN(fullName, "/", 2)
		ownerFromFull, repoFromFull = parts[0], parts[1]
	}
	resolvedOwner := owner
	if resolvedOwner == "" {
		resolvedOwner = ownerFromFull
	}
	if resolvedOwner == "" {
		resolvedOwner = viewerLogin
	}
	resolvedRepo := repoName
	if resolvedRepo == "" {
		resolvedRepo = repoFromFull
	}

	switch name {
	case "create_repo":
		repoNameArg, _ := mcpPickArg(args, "name").(string)
		if repoNameArg == "" {
			return mcpToolError("missing required argument: name")
		}
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		status, value := ctx.rest("POST", "/api/v3/user/repos", map[string]interface{}{"name": repoNameArg, "description": desc, "private": priv})
		if status != 201 {
			text, _ := marshalNoEscape(value)
			return mcpToolError("create_repo failed: " + string(text))
		}
		return mcpToolText(value)
	case "get_repo":
		if resolvedRepo == "" {
			return mcpToolError("missing required argument: repo")
		}
		status, value := ctx.rest("GET", "/api/v3/repos/"+resolvedOwner+"/"+resolvedRepo, nil)
		if status != 200 {
			text, _ := marshalNoEscape(value)
			return mcpToolError("get_repo failed: " + string(text))
		}
		return mcpToolText(value)
	case "create_issue":
		title, _ := mcpPickArg(args, "title").(string)
		if resolvedRepo == "" || title == "" {
			return mcpToolError("missing required argument: repo or title")
		}
		bodyStr, _ := args["body"].(string)
		status, value := ctx.rest("POST", "/api/v3/repos/"+resolvedOwner+"/"+resolvedRepo+"/issues", map[string]interface{}{"title": title, "body": bodyStr})
		if status != 201 {
			text, _ := marshalNoEscape(value)
			return mcpToolError("create_issue failed: " + string(text))
		}
		return mcpToolText(value)
	case "list_issues":
		if resolvedRepo == "" {
			return mcpToolError("missing required argument: repo")
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		status, value := ctx.rest("GET", "/api/v3/repos/"+resolvedOwner+"/"+resolvedRepo+"/issues?state="+url.QueryEscape(state), nil)
		if status != 200 {
			text, _ := marshalNoEscape(value)
			return mcpToolError("list_issues failed: " + string(text))
		}
		return mcpToolText(map[string]interface{}{"issues": value})
	case "create_pull_request":
		title, _ := mcpPickArg(args, "title").(string)
		head, _ := mcpPickArg(args, "head").(string)
		base, _ := mcpPickArg(args, "base").(string)
		if resolvedRepo == "" || title == "" || head == "" || base == "" {
			return mcpToolError("missing required argument: repo, title, head or base")
		}
		bodyStr, _ := args["body"].(string)
		status, value := ctx.rest("POST", "/api/v3/repos/"+resolvedOwner+"/"+resolvedRepo+"/pulls", map[string]interface{}{"title": title, "body": bodyStr, "head": head, "base": base})
		if status != 201 {
			text, _ := marshalNoEscape(value)
			return mcpToolError("create_pull_request failed: " + string(text))
		}
		return mcpToolText(value)
	case "merge_pull_request":
		pullNumber := mcpPickArg(args, "pull_number", "number")
		num, isNum := pullNumber.(float64)
		if resolvedRepo == "" || !isNum {
			return mcpToolError("missing required argument: repo or pull_number")
		}
		status, value := ctx.rest("PUT", "/api/v3/repos/"+resolvedOwner+"/"+resolvedRepo+"/pulls/"+strconv_Itoa(int(num))+"/merge", map[string]interface{}{"merge_method": "merge"})
		if status != 200 {
			text, _ := marshalNoEscape(value)
			return mcpToolError("merge_pull_request failed: " + string(text))
		}
		return mcpToolText(value)
	default:
		return nil
	}
}

func strconv_Itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// handleMcpLocked handles one MCP POST body. Caller holds the store lock.
func handleMcpLocked(ctx *mcpCtx, rawBody []byte) Result {
	parsed := jsonObjectBody(rawBody)
	if parsed == nil {
		return mcpJSON(400, map[string]interface{}{"jsonrpc": "2.0", "id": nil, "error": map[string]interface{}{"code": -32700, "message": "Parse error"}})
	}
	var id interface{}
	if v, ok := parsed["id"]; ok {
		id = v
	}
	method, _ := parsed["method"].(string)
	params, _ := parsed["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}
	meta, _ := params["_meta"].(map[string]interface{})
	if meta == nil {
		meta = map[string]interface{}{}
	}

	switch method {
	case "server/discover":
		return mcpRPCResult(id, map[string]interface{}{
			"supportedVersions": supportedVersions,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion},
			"_meta":             map[string]interface{}{"io.modelcontextprotocol/serverInfo": map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion}},
		})
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		version := supportedVersions[0]
		for _, v := range supportedVersions {
			if v == requested {
				version = v
				break
			}
		}
		return mcpRPCResult(id, map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion},
			"_meta":           map[string]interface{}{"io.modelcontextprotocol/serverInfo": map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion}},
		})
	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		return Result{Status: 202, Body: []byte{}, Extra: [][2]string{{"Content-Type", "application/json; charset=utf-8"}}}
	case "tools/list":
		return mcpRPCResult(id, map[string]interface{}{"tools": mcpTools(), "_meta": meta})
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]interface{})
		if args == nil {
			args = map[string]interface{}{}
		}
		var tool *mcpToolDef
		tools := mcpTools()
		for i := range tools {
			if tools[i].Name == name {
				tool = &tools[i]
				break
			}
		}
		if tool == nil {
			return mcpRPCError(id, -32602, "unknown tool: "+name)
		}
		for _, required := range tool.InputSchema.Required {
			v, present := args[required]
			if !present || v == "" {
				if required == "pull_number" {
					if _, ok := args["number"]; ok {
						continue
					}
				}
				return mcpRPCResult(id, mcpToolError("missing required argument: "+required))
			}
		}
		outcome := mcpCallTool(ctx, name, args)
		if outcome == nil {
			return mcpRPCError(id, -32602, "unknown tool: "+name)
		}
		return mcpRPCResult(id, outcome)
	default:
		return mcpRPCError(id, -32601, "method not found: "+method)
	}
}

// jsonObjectBody parses a JSON body like http.ts jsonObject: empty -> {},
// malformed or non-object (incl. array) -> nil.
func jsonObjectBody(body []byte) map[string]interface{} {
	if len(body) == 0 {
		return map[string]interface{}{}
	}
	var parsed interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&parsed); err != nil {
		return nil
	}
	m, ok := parsed.(map[string]interface{})
	if !ok {
		return nil
	}
	return m
}
