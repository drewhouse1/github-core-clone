// Model Context Protocol over streamable HTTP (JSON-RPC 2.0). Supports the
// 2026-07-28 era via server/discover and the legacy handshake via initialize.
// Ported from the Node mcp.ts; behavior is identical.

package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// McpTool is one tool definition.
type McpTool struct {
	Name        string
	Description string
	// properties in declaration order: name -> {type, description}
	Properties [][3]string
	Required   []string
}

var mcpTools = []McpTool{
	{
		Name:        "create_repo",
		Description: "Create a new repository for the authenticated user.",
		Properties: [][3]string{
			{"owner", "string", "The owner login of the repository."},
			{"name", "string", "The name of the repository."},
			{"description", "string", "A short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		},
		Required: []string{"name"},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		Properties: [][3]string{
			{"owner", "string", "The owner login of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name owner/repo of the repository."},
		},
		Required: []string{"owner", "repo"},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		Properties: [][3]string{
			{"owner", "string", "The owner login of the repository."},
			{"repo", "string", "The name of the repository."},
			{"title", "string", "The title of the issue."},
			{"body", "string", "The body of the issue."},
		},
		Required: []string{"owner", "repo", "title"},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		Properties: [][3]string{
			{"owner", "string", "The owner login of the repository."},
			{"repo", "string", "The name of the repository."},
			{"state", "string", "Issue state filter: open, closed, or all."},
		},
		Required: []string{"owner", "repo"},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request in a repository.",
		Properties: [][3]string{
			{"owner", "string", "The owner login of the repository."},
			{"repo", "string", "The name of the repository."},
			{"title", "string", "The title of the pull request."},
			{"body", "string", "The body of the pull request."},
			{"head", "string", "The head branch name."},
			{"base", "string", "The base branch name."},
		},
		Required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request in a repository.",
		Properties: [][3]string{
			{"owner", "string", "The owner login of the repository."},
			{"repo", "string", "The name of the repository."},
			{"pull_number", "integer", "The pull request number."},
		},
		Required: []string{"owner", "repo", "pull_number"},
	},
}

// mcpToolsListJSON is the prebuilt tools/list result value (byte-identical
// every call, like the Node TOOLS_LIST_JSON).
var mcpToolsListJVal *JVal

func init() {
	tools := []*JVal{}
	for _, t := range mcpTools {
		props := []JPair{}
		for _, p := range t.Properties {
			props = append(props, JField(p[0], JObj(JField("type", p[1]), JField("description", p[2]))))
		}
		req := []*JVal{}
		for _, r := range t.Required {
			req = append(req, JStr(r))
		}
		tools = append(tools, JObj(
			JField("name", t.Name),
			JField("description", t.Description),
			JField("inputSchema", JObj(
				JField("type", "object"),
				JField("properties", JObj(props...)),
				JField("required", JArr(req...)),
			)),
		))
	}
	mcpToolsListJVal = JObj(JField("tools", JArr(tools...)))
}

func mcpNorm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func mcpFindTool(name string) *McpTool {
	n := mcpNorm(name)
	for i := range mcpTools {
		if mcpNorm(mcpTools[i].Name) == n {
			return &mcpTools[i]
		}
	}
	for i := range mcpTools {
		if strings.HasSuffix(n, mcpNorm(mcpTools[i].Name)) {
			return &mcpTools[i]
		}
	}
	return nil
}

var mcpSynonyms = map[string][]string{
	"owner":       {"owner", "owner_login", "org", "organization", "organisation", "user", "user_login", "login", "account", "namespace", "owner_name"},
	"repo":        {"repo", "repository", "repository_name", "repo_name", "repo_slug", "project", "project_name"},
	"title":       {"title", "issue_title", "pr_title", "pull_request_title", "subject"},
	"body":        {"body", "issue_body", "description", "content", "text", "comment"},
	"head":        {"head", "head_ref", "head_branch", "pull_request_head", "pr_head", "source", "source_ref", "source_branch", "from", "from_branch", "branch"},
	"base":        {"base", "base_ref", "base_branch", "pull_request_base", "pr_base", "target", "target_ref", "target_branch", "into", "into_branch"},
	"pull_number": {"pull_number", "pull_request_number", "pr_number", "number", "issue_number", "index"},
	"name":        {"name", "repo", "repository", "repository_name", "repo_name"},
	"description": {"description", "about", "summary", "body"},
	"private":     {"private", "is_private"},
	"state":       {"state", "status"},
}

// mcpMapArgs maps the suite's documented argument names onto the tool schema.
func mcpMapArgs(tool *McpTool, args map[string]any) map[string]any {
	out := map[string]any{}
	byNorm := map[string]string{}
	for _, p := range tool.Properties {
		byNorm[mcpNorm(p[0])] = p[0]
	}
	// First pass: exact normalized matches.
	for k, v := range args {
		if prop, ok := byNorm[mcpNorm(k)]; ok {
			out[prop] = v
		}
	}
	// Second pass: synonyms for properties still missing.
	for _, p := range tool.Properties {
		prop := p[0]
		if _, ok := out[prop]; ok {
			continue
		}
		syns, ok := mcpSynonyms[prop]
		if !ok {
			syns = []string{prop}
		}
		for _, s := range syns {
			for k, v := range args {
				if mcpNorm(k) == mcpNorm(s) {
					out[prop] = v
					break
				}
			}
			if _, ok := out[prop]; ok {
				break
			}
		}
	}
	// full_name fallback for owner/repo (fills whichever is missing).
	fillFromFullName := func(key string) {
		_, hasOwner := out["owner"]
		_, hasRepo := out["repo"]
		if hasOwner && hasRepo {
			return
		}
		if fn, ok := args[key].(string); ok {
			parts := strings.Split(fn, "/")
			if len(parts) == 2 {
				if !hasOwner {
					out["owner"] = parts[0]
				}
				if !hasRepo {
					out["repo"] = parts[1]
				}
			}
		}
	}
	fillFromFullName("full_name")
	fillFromFullName("repo_full_name")
	return out
}

// McpResult is one MCP handler result.
type McpResult struct {
	Status  int
	Body    *JVal // nil for a 202 notification ack
	Headers map[string]string
}

func mcpRPCResult(id *JVal, result *JVal) McpResult {
	return McpResult{Status: 200, Body: JObj(
		JField("jsonrpc", "2.0"),
		JField("id", id),
		JField("result", result),
	)}
}

func mcpRPCError(id *JVal, code int, message string) McpResult {
	return McpResult{Status: 200, Body: JObj(
		JField("jsonrpc", "2.0"),
		JField("id", id),
		JField("error", JObj(JField("code", float64(code)), JField("message", message))),
	)}
}

func mcpToolResult(data *JVal) *JVal {
	return JObj(
		JField("content", JArr(JObj(JField("type", "text"), JField("text", string(data.Marshal()))))),
		JField("structuredContent", data),
		JField("isError", JBool(false)),
	)
}

func mcpToolError(message string) *JVal {
	return JObj(
		JField("content", JArr(JObj(JField("type", "text"), JField("text", message)))),
		JField("isError", JBool(true)),
	)
}

// jsString mimics JS String(v) for the values the suite sends.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return jsNumberString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func jsNumberString(n float64) string {
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// mcpCallTool executes one tools/call; id is stamped 0 and rewritten by the
// caller (matching the Node callTool, which always writes id 0).
func mcpCallTool(store *Store, tool *McpTool, rawArgs map[string]any) McpResult {
	args := mcpMapArgs(tool, rawArgs)
	zero := JNum(0)
	for _, req := range tool.Required {
		v := args[req]
		if v == nil || v == "" {
			return McpResult{Status: 200, Body: JObj(
				JField("jsonrpc", "2.0"),
				JField("id", zero),
				JField("result", mcpToolError("Missing required argument: "+req)),
			)}
		}
	}
	owner := store.ViewerLogin
	if s, ok := args["owner"].(string); ok && s != "" {
		owner = s
	}
	repoName := ""
	if s, ok := args["repo"].(string); ok {
		repoName = s
	} else if s, ok := args["name"].(string); ok {
		repoName = s
	}
	wrap := func(result *JVal) McpResult {
		return McpResult{Status: 200, Body: JObj(
			JField("jsonrpc", "2.0"),
			JField("id", zero),
			JField("result", result),
		)}
	}
	switch tool.Name {
	case "create_repo":
		name := jsString(args["name"])
		if name == "" {
			return wrap(mcpToolError("Missing required argument: name"))
		}
		existing := store.GetRepo(owner, name)
		var repo *Repo
		if existing != nil {
			repo = existing
		} else {
			desc := ""
			if s, ok := args["description"].(string); ok {
				desc = s
			}
			priv, _ := args["private"].(bool)
			repo = store.CreateRepo(owner, name, desc, priv)
		}
		return wrap(mcpToolResult(repoJSON(store, repo)))
	case "get_repo":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return wrap(mcpToolError("Repository " + owner + "/" + repoName + " not found"))
		}
		return wrap(mcpToolResult(repoJSON(store, repo)))
	case "create_issue":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return wrap(mcpToolError("Repository " + owner + "/" + repoName + " not found"))
		}
		store.IssueIDSeq++
		repo.IssueSeq++
		seq := store.NextSeq()
		body := ""
		if s, ok := args["body"].(string); ok {
			body = s
		}
		issue := &Issue{ID: store.IssueIDSeq, Number: repo.IssueSeq, Title: jsString(args["title"]), Body: body, State: "open", Labels: []string{}, Seq: seq, Comments: []*Comment{}}
		repo.Issues[issue.Number] = issue
		return wrap(mcpToolResult(issueJSON(store, repo, issue)))
	case "list_issues":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return wrap(mcpToolError("Repository " + owner + "/" + repoName + " not found"))
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = s
		}
		list := []*Issue{}
		for _, i := range repo.Issues {
			list = append(list, i)
		}
		if state != "all" {
			filtered := []*Issue{}
			for _, i := range list {
				if i.State == state {
					filtered = append(filtered, i)
				}
			}
			list = filtered
		}
		sortIssuesBySeq(list, false)
		items := []*JVal{}
		for _, i := range list {
			items = append(items, issueJSON(store, repo, i))
		}
		return wrap(mcpToolResult(JArr(items...)))
	case "create_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return wrap(mcpToolError("Repository " + owner + "/" + repoName + " not found"))
		}
		head := jsString(args["head"])
		base := jsString(args["base"])
		if base == "" {
			base = repo.DefaultBranch
		}
		headSha, hasHead := repo.Refs["refs/heads/"+head]
		baseSha, hasBase := repo.Refs["refs/heads/"+base]
		if !hasHead || !hasBase {
			return wrap(mcpToolError("head or base branch not found"))
		}
		comp := store.ComputePull(repo, headSha, baseSha)
		store.PullIDSeq++
		repo.PullSeq++
		seq := store.NextSeq()
		body := ""
		if s, ok := args["body"].(string); ok {
			body = s
		}
		pull := &Pull{
			ID: store.PullIDSeq, Number: repo.PullSeq, Title: jsString(args["title"]), Body: body,
			State: "open", Merged: false, MergedAt: "", MergeCommitSha: "",
			HeadRef: head, BaseRef: base, HeadSha: headSha, BaseSha: baseSha,
			MergeBaseSha: comp.MergeBaseSha, Files: comp.Files, Seq: seq,
		}
		repo.Pulls[pull.Number] = pull
		return wrap(mcpToolResult(pullJSON(store, repo, pull)))
	case "merge_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return wrap(mcpToolError("Repository " + owner + "/" + repoName + " not found"))
		}
		num := 0
		if n, ok := gqlToIntOk(args["pull_number"]); ok {
			num = n
		}
		pull := repo.Pulls[num]
		if pull == nil {
			return wrap(mcpToolError("Pull request not found"))
		}
		ok, sha, msg := mergePullRequest(store, repo, pull)
		if !ok {
			if msg == "" {
				msg = "merge failed"
			}
			return wrap(mcpToolError(msg))
		}
		return wrap(mcpToolResult(JObj(
			JField("number", float64(pull.Number)),
			JField("merged", JBool(true)),
			JField("sha", sha),
			JField("state", "closed"),
		)))
	default:
		return wrap(mcpToolError("Unknown tool " + tool.Name))
	}
}

var mcpIDRe = regexp.MustCompile(`"id"\s*:\s*("([^"]*)"|(-?\d+))`)
var mcpVersionRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// handleMcp handles one MCP POST. rawBody is the request bytes.
func handleMcp(store *Store, rawBody []byte) McpResult {
	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		// Try to recover the id for the parse-error response.
		text := string(rawBody)
		var id *JVal = JNull()
		if m := mcpIDRe.FindStringSubmatch(text); m != nil {
			if m[2] != "" {
				id = JStr(m[2])
			} else if m[3] != "" {
				if n, err := strconv.ParseFloat(m[3], 64); err == nil {
					id = JNum(int64(n))
				}
			}
		}
		return McpResult{Status: 400, Body: JObj(
			JField("jsonrpc", "2.0"),
			JField("id", id),
			JField("error", JObj(JField("code", float64(-32700)), JField("message", "Parse error"))),
		)}
	}
	req, ok := parsed.(map[string]any)
	if !ok {
		return McpResult{Status: 400, Body: JObj(
			JField("jsonrpc", "2.0"),
			JField("id", JNull()),
			JField("error", JObj(JField("code", float64(-32700)), JField("message", "Parse error"))),
		)}
	}
	var id *JVal
	hasID := false
	if v, ok := req["id"]; ok {
		hasID = true
		id = anyToJVal(v)
	} else {
		id = JNull()
	}
	method := ""
	if s, ok := req["method"].(string); ok {
		method = s
	}
	params := map[string]any{}
	if p, ok := req["params"].(map[string]any); ok {
		params = p
	}

	// Notification (no id): acknowledge, no body.
	if !hasID {
		return McpResult{Status: 202, Body: nil}
	}

	if method == "server/discover" {
		res := mcpRPCResult(id, JObj(
			JField("supportedVersions", JArr(JStr("2026-07-28"))),
			JField("capabilities", JObj(JField("tools", JObj()))),
			JField("serverInfo", JObj(JField("name", "github-forge"), JField("version", "1.0.0"))),
			JField("instructions", "A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools."),
			JField("_meta", JObj(JField("io.modelcontextprotocol/serverInfo", JObj(JField("name", "github-forge"), JField("version", "1.0.0"))))),
		))
		res.Headers = map[string]string{"mcp-session-id": "forge-1"}
		return res
	}

	if method == "initialize" {
		offered := "2026-07-28"
		if s, ok := params["protocolVersion"].(string); ok && mcpVersionRe.MatchString(s) {
			offered = s
		}
		res := mcpRPCResult(id, JObj(
			JField("protocolVersion", offered),
			JField("capabilities", JObj(JField("tools", JObj()))),
			JField("serverInfo", JObj(JField("name", "github-forge"), JField("version", "1.0.0"))),
			JField("_meta", JObj(JField("io.modelcontextprotocol/serverInfo", JObj(JField("name", "github-forge"), JField("version", "1.0.0"))))),
		))
		res.Headers = map[string]string{"mcp-session-id": "forge-1"}
		return res
	}

	if method == "tools/list" {
		return McpResult{Status: 200, Body: JObj(
			JField("jsonrpc", "2.0"),
			JField("id", id),
			JField("result", mcpToolsListJVal),
		)}
	}

	if method == "tools/call" {
		name := ""
		if s, ok := params["name"].(string); ok {
			name = s
		}
		argsRaw, hasArgs := params["arguments"]
		args, okArgs := argsRaw.(map[string]any)
		if !hasArgs {
			args = map[string]any{}
			okArgs = true
		}
		if !okArgs {
			return mcpRPCError(id, -32602, "Invalid params: arguments must be an object")
		}
		tool := mcpFindTool(name)
		if tool == nil {
			return mcpRPCError(id, -32602, "Unknown tool: "+name)
		}
		res := mcpCallTool(store, tool, args)
		// callTool stamps id 0; rewrite to the caller's id.
		body := res.Body
		var result, errVal *JVal
		if body != nil {
			for _, f := range body.obj {
				if f.Key == "result" {
					result = f.Val
				}
				if f.Key == "error" {
					errVal = f.Val
				}
			}
		}
		fields := []JPair{JField("jsonrpc", "2.0"), JField("id", id)}
		if result != nil {
			fields = append(fields, JField("result", result))
		}
		if errVal != nil {
			fields = append(fields, JField("error", errVal))
		}
		return McpResult{Status: res.Status, Body: JObj(fields...)}
	}

	if method == "ping" {
		return mcpRPCResult(id, JObj())
	}
	if method == "resources/list" {
		return mcpRPCResult(id, JObj(JField("resources", JArr())))
	}
	if method == "prompts/list" {
		return mcpRPCResult(id, JObj(JField("prompts", JArr())))
	}

	return mcpRPCError(id, -32601, "Method not found: "+method)
}

// anyToJVal converts a parsed JSON value into an ordered JVal.
func anyToJVal(v any) *JVal {
	switch x := v.(type) {
	case nil:
		return JNull()
	case bool:
		return JBool(x)
	case float64:
		return JNumF(x)
	case string:
		return JStr(x)
	case []any:
		items := make([]*JVal, 0, len(x))
		for _, e := range x {
			items = append(items, anyToJVal(e))
		}
		return JArr(items...)
	case map[string]any:
		// JSON object key order is lost in Go's map; the suite only sends
		// id as string/number, which this handles. Object values are not
		// re-serialized for ids.
		fields := []JPair{}
		for k, e := range x {
			fields = append(fields, JField(k, anyToJVal(e)))
		}
		return JObj(fields...)
	}
	return JNull()
}
