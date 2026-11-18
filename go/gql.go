// GitHub GraphQL v4: a small parser + executor for exactly the documents the
// suite sends (gh / github-mcp-server shapes). Ported from the Node gql.ts;
// behavior is identical, including field order and error strings.

package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- parser

// ArgValue is one parsed GraphQL argument value.
type ArgValue struct {
	Kind   string // var, string, int, float, bool, null, enum, list, object
	Str    string
	Num    float64
	Bool   bool
	Items  []*ArgValue
	Fields map[string]*ArgValue
}

// Selection is one field selection (or inline fragment, Name == "__fragment").
type Selection struct {
	Alias        string
	HasAlias     bool
	Name         string
	Args         map[string]*ArgValue
	Selections   []*Selection // nil = no sub-selection
	FragmentType string
	HasFragment  bool
}

// Operation is one parsed query/mutation document.
type Operation struct {
	Kind       string // query | mutation
	Name       string
	HasName    bool
	Selections []*Selection
}

type gqlToken struct {
	kind  string // name, string, int, float, punct, spread
	value string
}

func gqlTokenize(src string) ([]gqlToken, error) {
	tokens := []gqlToken{}
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == ';' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+2 < len(src)+1 && strings.HasPrefix(src[i:], "...") {
			tokens = append(tokens, gqlToken{kind: "spread", value: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$@&|", ch) >= 0 {
			tokens = append(tokens, gqlToken{kind: "punct", value: string(ch)})
			i++
			continue
		}
		if ch == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: "string", value: out.String()})
			i = j + 1
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			text := src[i:j]
			kind := "int"
			if strings.ContainsAny(text, ".eE") {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind: kind, value: text})
			i = j
			continue
		}
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{kind: "name", value: src[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %s in GraphQL document", JString(string(ch)))
	}
	return tokens, nil
}

type gqlFragment struct {
	typ        string
	selections []*Selection
}

type gqlParser struct {
	tokens    []gqlToken
	pos       int
	fragments map[string]*gqlFragment
}

func (p *gqlParser) peek() *gqlToken {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, fmt.Errorf("unexpected end of GraphQL document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != "punct" || t.value != v {
		return fmt.Errorf("expected '%s' in GraphQL document, got %s", v, t.value)
	}
	return nil
}

func (p *gqlParser) expectName(want string) (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != "name" {
		return "", fmt.Errorf("expected a name in GraphQL document, got %s", t.value)
	}
	if want != "" && t.value != want {
		return "", fmt.Errorf("expected name '%s', got %s", want, t.value)
	}
	return t.value, nil
}

func (p *gqlParser) parseValue() (*ArgValue, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == "punct" && t.value == "$" {
		name, err := p.expectName("")
		if err != nil {
			return nil, err
		}
		return &ArgValue{Kind: "var", Str: name}, nil
	}
	if t.kind == "string" {
		return &ArgValue{Kind: "string", Str: t.value}, nil
	}
	if t.kind == "int" {
		n, _ := strconv.ParseInt(t.value, 10, 64)
		return &ArgValue{Kind: "int", Num: float64(n)}, nil
	}
	if t.kind == "float" {
		n, _ := strconv.ParseFloat(t.value, 64)
		return &ArgValue{Kind: "float", Num: n}, nil
	}
	if t.kind == "punct" && t.value == "[" {
		items := []*ArgValue{}
		for {
			pk := p.peek()
			if pk != nil && pk.kind == "punct" && pk.value == "]" {
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return &ArgValue{Kind: "list", Items: items}, nil
	}
	if t.kind == "punct" && t.value == "{" {
		fields := map[string]*ArgValue{}
		for {
			pk := p.peek()
			if pk != nil && pk.kind == "punct" && pk.value == "}" {
				break
			}
			name, err := p.expectName("")
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
			fields[name] = v
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return &ArgValue{Kind: "object", Fields: fields}, nil
	}
	if t.kind == "name" {
		switch t.value {
		case "true":
			return &ArgValue{Kind: "bool", Bool: true}, nil
		case "false":
			return &ArgValue{Kind: "bool", Bool: false}, nil
		case "null":
			return &ArgValue{Kind: "null"}, nil
		}
		return &ArgValue{Kind: "enum", Str: t.value}, nil
	}
	return nil, fmt.Errorf("unexpected token %s in GraphQL value", t.value)
}

func (p *gqlParser) parseArgs() (map[string]*ArgValue, error) {
	args := map[string]*ArgValue{}
	pk := p.peek()
	if pk != nil && pk.kind == "punct" && pk.value == "(" {
		p.next()
		for {
			pk := p.peek()
			if pk != nil && pk.kind == "punct" && pk.value == ")" {
				break
			}
			name, err := p.expectName("")
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
			args[name] = v
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func (p *gqlParser) parseSelections() ([]*Selection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	out := []*Selection{}
	for {
		pk := p.peek()
		if pk != nil && pk.kind == "punct" && pk.value == "}" {
			break
		}
		t := p.peek()
		if t != nil && t.kind == "spread" {
			p.next()
			n := p.peek()
			if n != nil && n.kind == "name" && n.value == "on" {
				p.next()
				typeName, err := p.expectName("")
				if err != nil {
					return nil, err
				}
				sels, err := p.parseSelections()
				if err != nil {
					return nil, err
				}
				out = append(out, &Selection{Name: "__fragment", Selections: sels, FragmentType: typeName, HasFragment: true})
			} else {
				fragName, err := p.expectName("")
				if err != nil {
					return nil, err
				}
				if frag, ok := p.fragments[fragName]; ok {
					out = append(out, &Selection{Name: "__fragment", Selections: frag.selections, FragmentType: frag.typ, HasFragment: true})
				}
			}
			continue
		}
		first, err := p.expectName("")
		if err != nil {
			return nil, err
		}
		alias := ""
		hasAlias := false
		name := first
		pk = p.peek()
		if pk != nil && pk.kind == "punct" && pk.value == ":" {
			p.next()
			alias = first
			hasAlias = true
			name, err = p.expectName("")
			if err != nil {
				return nil, err
			}
		}
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		var sels []*Selection
		pk = p.peek()
		if pk != nil && pk.kind == "punct" && pk.value == "{" {
			sels, err = p.parseSelections()
			if err != nil {
				return nil, err
			}
		}
		out = append(out, &Selection{Alias: alias, HasAlias: hasAlias, Name: name, Args: args, Selections: sels})
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseVariableDefinitions() error {
	pk := p.peek()
	if pk != nil && pk.kind == "punct" && pk.value == "(" {
		p.next()
		depth := 1
		for depth > 0 {
			t, err := p.next()
			if err != nil {
				return err
			}
			if t.kind == "punct" && t.value == "(" {
				depth++
			}
			if t.kind == "punct" && t.value == ")" {
				depth--
			}
		}
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]*Operation, error) {
	ops := []*Operation{}
	for p.pos < len(p.tokens) {
		t := p.peek()
		if t == nil {
			break
		}
		if t.kind == "name" && t.value == "fragment" {
			p.next()
			name, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			if _, err := p.expectName("on"); err != nil {
				return nil, err
			}
			typ, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			p.fragments[name] = &gqlFragment{typ: typ, selections: sels}
			continue
		}
		kind := "query"
		name := ""
		hasName := false
		if t.kind == "punct" && t.value == "{" {
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &Operation{Kind: kind, Selections: sels})
			continue
		}
		if t.kind == "name" && (t.value == "query" || t.value == "mutation") {
			kind = t.value
			p.next()
			n := p.peek()
			if n != nil && n.kind == "name" {
				tok, err := p.next()
				if err != nil {
					return nil, err
				}
				name = tok.value
				hasName = true
			}
			if err := p.parseVariableDefinitions(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &Operation{Kind: kind, Name: name, HasName: hasName, Selections: sels})
			continue
		}
		return nil, fmt.Errorf("unexpected token %s in GraphQL document", t.value)
	}
	return ops, nil
}

// ---------------------------------------------------------------- execution

// GqlError is one GraphQL error object.
type GqlError struct {
	Message string
}

var gqlTypeFields = map[string][]string{
	"User":                   {"login", "id", "name", "email"},
	"Repository":             {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"Issue":                  {"id", "number", "title", "state", "body"},
	"PullRequest":            {"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "body"},
	"IssueConnection":        {"nodes", "pageInfo"},
	"PullRequestConnection":  {"nodes"},
	"PullRequestChangedFile": {"path"},
	"PageInfo":               {"hasNextPage", "endCursor"},
	"Comment":                {"id", "body"},
	"CommentEdge":            {"node"},
	"Ref":                    {"name"},
	"__Type":                 {"name", "fields", "enumValues"},
	"__Field":                {"name"},
	"__EnumValue":            {"name"},
}

var gqlSearchTypeValues = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}

func gqlResolveArg(v *ArgValue, vars map[string]any) any {
	switch v.Kind {
	case "var":
		return vars[v.Str]
	case "string":
		return v.Str
	case "int", "float":
		return v.Num
	case "bool":
		return v.Bool
	case "null":
		return nil
	case "enum":
		return v.Str
	case "list":
		out := make([]any, 0, len(v.Items))
		for _, x := range v.Items {
			out = append(out, gqlResolveArg(x, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, val := range v.Fields {
			out[k] = gqlResolveArg(val, vars)
		}
		return out
	}
	return nil
}

func gqlResolveArgs(sel *Selection, vars map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range sel.Args {
		out[k] = gqlResolveArg(v, vars)
	}
	return out
}

func gqlCursorForNumber(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("c" + strconv.Itoa(n)))
}

var gqlCursorRe = regexp.MustCompile(`^c(\d+)$`)

func gqlNumberFromCursor(c string) (int, bool) {
	if c == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	m := gqlCursorRe.FindStringSubmatch(string(raw))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// gqlObj is an executor-side object: a type tag plus its payload.
type gqlObj struct {
	typ     string
	repo    *Repo
	issue   *Issue
	pull    *Pull
	user    *gqlUser
	comment *Comment
	edge    *gqlObj
	name    string // Ref name / __Type name
}

type gqlUser struct {
	login string
	id    int
	name  string
	email string
}

// gqlExecutor executes operations against a Store.
type gqlExecutor struct {
	store  *Store
	errors []GqlError
}

func (e *gqlExecutor) repoByGlobalID(id string) *Repo {
	m := regexp.MustCompile(`^R(\d+)$`).FindStringSubmatch(id)
	if m == nil {
		return nil
	}
	dbid, _ := strconv.Atoi(m[1])
	for _, r := range e.store.Repos {
		if r.ID == dbid {
			return r
		}
	}
	return nil
}

func (e *gqlExecutor) issueByGlobalID(id string) (*Repo, *Issue) {
	m := regexp.MustCompile(`^I(\d+)_(\d+)$`).FindStringSubmatch(id)
	if m == nil {
		return nil, nil
	}
	repo := e.repoByGlobalID("R" + m[1])
	if repo == nil {
		return nil, nil
	}
	num, _ := strconv.Atoi(m[2])
	issue := repo.Issues[num]
	if issue == nil {
		return nil, nil
	}
	return repo, issue
}

func (e *gqlExecutor) pullByGlobalID(id string) (*Repo, *Pull) {
	m := regexp.MustCompile(`^P(\d+)_(\d+)$`).FindStringSubmatch(id)
	if m == nil {
		return nil, nil
	}
	repo := e.repoByGlobalID("R" + m[1])
	if repo == nil {
		return nil, nil
	}
	num, _ := strconv.Atoi(m[2])
	pull := repo.Pulls[num]
	if pull == nil {
		return nil, nil
	}
	return repo, pull
}

func (e *gqlExecutor) executeOperation(op *Operation, variables map[string]any) *JVal {
	e.errors = nil
	fields := []JPair{}
	for _, sel := range op.Selections {
		key := sel.Name
		if sel.HasAlias {
			key = sel.Alias
		}
		fields = append(fields, JField(key, e.resolveRoot(sel, op.Kind, variables)))
	}
	data := JObj(fields...)
	if len(e.errors) > 0 {
		errs := []*JVal{}
		for _, er := range e.errors {
			errs = append(errs, JObj(JField("message", er.Message)))
		}
		return JObj(JField("data", data), JField("errors", JArr(errs...)))
	}
	return JObj(JField("data", data))
}

func (e *gqlExecutor) resolveRoot(sel *Selection, kind string, vars map[string]any) *JVal {
	args := gqlResolveArgs(sel, vars)
	if kind == "query" {
		switch sel.Name {
		case "viewer":
			u := &gqlUser{login: e.store.ViewerLogin, id: e.store.ViewerID, name: e.store.ViewerName, email: e.store.ViewerEmail}
			return e.resolveSelections(&gqlObj{typ: "User", user: u}, sel.Selections, vars)
		case "repository":
			owner := gqlToString(args["owner"])
			name := gqlToString(args["name"])
			if owner != e.store.ViewerLogin {
				return JNull()
			}
			repo := e.store.GetRepo(owner, name)
			if repo == nil {
				return JNull()
			}
			return e.resolveSelections(&gqlObj{typ: "Repository", repo: repo}, sel.Selections, vars)
		case "node":
			id := gqlToString(args["id"])
			if repo := e.repoByGlobalID(id); repo != nil {
				return e.resolveSelections(&gqlObj{typ: "Repository", repo: repo}, sel.Selections, vars)
			}
			if repo, issue := e.issueByGlobalID(id); issue != nil {
				return e.resolveSelections(&gqlObj{typ: "Issue", repo: repo, issue: issue}, sel.Selections, vars)
			}
			if repo, pull := e.pullByGlobalID(id); pull != nil {
				return e.resolveSelections(&gqlObj{typ: "PullRequest", repo: repo, pull: pull}, sel.Selections, vars)
			}
			return JNull()
		case "__type":
			name := gqlToString(args["name"])
			return e.resolveSelections(&gqlObj{typ: "__Type", name: name}, sel.Selections, vars)
		case "__schema":
			return JNull()
		}
		e.errors = append(e.errors, GqlError{Message: `Cannot query field "` + sel.Name + `" on type "Query".`})
		return JNull()
	}
	// Mutations
	switch sel.Name {
	case "createIssue":
		return e.mutateCreateIssue(sel, args, vars)
	case "closeIssue":
		return e.mutateCloseIssue(sel, args, vars)
	case "createPullRequest":
		return e.mutateCreatePullRequest(sel, args, vars)
	case "mergePullRequest":
		return e.mutateMergePullRequest(sel, args, vars)
	case "addComment":
		return e.mutateAddComment(sel, args, vars)
	}
	e.errors = append(e.errors, GqlError{Message: `Cannot query field "` + sel.Name + `" on type "Mutation".`})
	return JNull()
}

func (e *gqlExecutor) mutateCreateIssue(sel *Selection, args map[string]any, vars map[string]any) *JVal {
	input := gqlToMap(args["input"])
	repo := e.repoByGlobalID(gqlToString(input["repositoryId"]))
	if repo == nil {
		e.errors = append(e.errors, GqlError{Message: "Repository not found"})
		return JNull()
	}
	e.store.IssueIDSeq++
	repo.IssueSeq++
	seq := e.store.NextSeq()
	issue := &Issue{ID: e.store.IssueIDSeq, Number: repo.IssueSeq, Title: gqlToString(input["title"]), Body: gqlToString(input["body"]), State: "open", Labels: []string{}, Seq: seq, Comments: []*Comment{}}
	repo.Issues[issue.Number] = issue
	return e.resolveSelections(&gqlObj{typ: "CreateIssuePayload", issue: issue, repo: repo}, sel.Selections, vars)
}

func (e *gqlExecutor) mutateCloseIssue(sel *Selection, args map[string]any, vars map[string]any) *JVal {
	input := gqlToMap(args["input"])
	repo, issue := e.issueByGlobalID(gqlToString(input["issueId"]))
	if issue == nil {
		e.errors = append(e.errors, GqlError{Message: "Issue not found"})
		return JNull()
	}
	issue.State = "closed"
	issue.JSONCacheSet = false
	issue.JSONCache = ""
	return e.resolveSelections(&gqlObj{typ: "CloseIssuePayload", issue: issue, repo: repo}, sel.Selections, vars)
}

func (e *gqlExecutor) mutateCreatePullRequest(sel *Selection, args map[string]any, vars map[string]any) *JVal {
	input := gqlToMap(args["input"])
	repo := e.repoByGlobalID(gqlToString(input["repositoryId"]))
	if repo == nil {
		e.errors = append(e.errors, GqlError{Message: "Repository not found"})
		return JNull()
	}
	headRef := gqlToString(input["headRefName"])
	baseRef := gqlToString(input["baseRefName"])
	if baseRef == "" {
		baseRef = repo.DefaultBranch
	}
	headSha, hasHead := repo.Refs["refs/heads/"+headRef]
	baseSha, hasBase := repo.Refs["refs/heads/"+baseRef]
	if !hasHead || !hasBase {
		e.errors = append(e.errors, GqlError{Message: "head or base branch not found"})
		return JNull()
	}
	comp := e.store.ComputePull(repo, headSha, baseSha)
	e.store.PullIDSeq++
	repo.PullSeq++
	seq := e.store.NextSeq()
	pull := &Pull{
		ID: e.store.PullIDSeq, Number: repo.PullSeq, Title: gqlToString(input["title"]), Body: "",
		State: "open", Merged: false, MergedAt: "", MergeCommitSha: "",
		HeadRef: headRef, BaseRef: baseRef, HeadSha: headSha, BaseSha: baseSha,
		MergeBaseSha: comp.MergeBaseSha, Files: comp.Files, Seq: seq,
	}
	repo.Pulls[pull.Number] = pull
	return e.resolveSelections(&gqlObj{typ: "CreatePullRequestPayload", pull: pull, repo: repo}, sel.Selections, vars)
}

func (e *gqlExecutor) mutateMergePullRequest(sel *Selection, args map[string]any, vars map[string]any) *JVal {
	input := gqlToMap(args["input"])
	repo, pull := e.pullByGlobalID(gqlToString(input["pullRequestId"]))
	if pull == nil {
		e.errors = append(e.errors, GqlError{Message: "Pull request not found"})
		return JNull()
	}
	if pull.Merged || pull.State != "open" {
		e.errors = append(e.errors, GqlError{Message: "Pull request is not open"})
		return JNull()
	}
	headSha, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
	baseSha, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
	if !hasHead || !hasBase {
		e.errors = append(e.errors, GqlError{Message: "head or base branch not found"})
		return JNull()
	}
	comp := e.store.ComputePull(repo, headSha, baseSha)
	if comp.Conflict {
		e.errors = append(e.errors, GqlError{Message: "Merge conflict"})
		return JNull()
	}
	headCommit := repo.Objects[headSha]
	var headTree string
	if headCommit != nil && headCommit.Type == ObjCommit {
		headTree = e.store.ParseCommit(headCommit.Data).Tree
	}
	if headTree == "" {
		e.errors = append(e.errors, GqlError{Message: "head has no tree"})
		return JNull()
	}
	seq := e.store.NextSeq()
	mergeSha := e.store.CreateCommit(repo, headTree, []string{baseSha}, "Merge pull request #"+strconv.Itoa(pull.Number)+": "+pull.Title, e.store.TsEpoch(seq))
	repo.Refs["refs/heads/"+pull.BaseRef] = mergeSha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = e.store.TsIso(seq)
	pull.MergeCommitSha = mergeSha
	pull.BaseSha = mergeSha
	return e.resolveSelections(&gqlObj{typ: "MergePullRequestPayload", pull: pull, repo: repo}, sel.Selections, vars)
}

func (e *gqlExecutor) mutateAddComment(sel *Selection, args map[string]any, vars map[string]any) *JVal {
	input := gqlToMap(args["input"])
	repo, issue := e.issueByGlobalID(gqlToString(input["subjectId"]))
	if issue == nil {
		e.errors = append(e.errors, GqlError{Message: "Subject not found"})
		return JNull()
	}
	e.store.CommentIDSeq++
	seq := e.store.NextSeq()
	comment := &Comment{ID: e.store.CommentIDSeq, Body: gqlToString(input["body"]), Seq: seq}
	issue.Comments = append(issue.Comments, comment)
	issue.JSONCacheSet = false
	issue.JSONCache = ""
	edge := &gqlObj{typ: "CommentEdge", comment: comment, repo: repo}
	return e.resolveSelections(&gqlObj{typ: "AddCommentPayload", edge: edge}, sel.Selections, vars)
}

func (e *gqlExecutor) resolveSelections(obj *gqlObj, selections []*Selection, vars map[string]any) *JVal {
	if selections == nil {
		return JNull()
	}
	fields := []JPair{}
	for _, sel := range selections {
		if sel.Name == "__fragment" {
			if sel.HasFragment && sel.FragmentType != "" && sel.FragmentType != obj.typ && sel.FragmentType != "Node" {
				continue
			}
			sub := e.resolveSelections(obj, sel.Selections, vars)
			if sub != nil && sub.kind == 'o' {
				fields = append(fields, sub.obj...)
			}
			continue
		}
		key := sel.Name
		if sel.HasAlias {
			key = sel.Alias
		}
		fields = append(fields, JField(key, e.resolveField(obj, sel, vars)))
	}
	return JObj(fields...)
}

func (e *gqlExecutor) resolveField(obj *gqlObj, sel *Selection, vars map[string]any) *JVal {
	typ := obj.typ
	known := gqlTypeFields[typ]
	// Payload types expose their nested object under its field name.
	if strings.HasSuffix(typ, "Payload") {
		if sel.Name == "issue" && obj.issue != nil {
			return e.resolveSelections(&gqlObj{typ: "Issue", repo: obj.repo, issue: obj.issue}, sel.Selections, vars)
		}
		if sel.Name == "pullRequest" && obj.pull != nil {
			return e.resolveSelections(&gqlObj{typ: "PullRequest", repo: obj.repo, pull: obj.pull}, sel.Selections, vars)
		}
		if sel.Name == "commentEdge" && obj.edge != nil {
			return e.resolveSelections(obj.edge, sel.Selections, vars)
		}
		e.errors = append(e.errors, GqlError{Message: `Cannot query field "` + sel.Name + `" on type "` + typ + `".`})
		return JNull()
	}
	found := false
	for _, f := range known {
		if f == sel.Name {
			found = true
			break
		}
	}
	if !found {
		e.errors = append(e.errors, GqlError{Message: `Cannot query field "` + sel.Name + `" on type "` + typ + `".`})
		return JNull()
	}
	args := gqlResolveArgs(sel, vars)
	switch typ {
	case "User":
		u := obj.user
		switch sel.Name {
		case "login":
			return JStr(u.login)
		case "id":
			return JStr("U" + strconv.Itoa(u.id))
		case "name":
			return JStr(u.name)
		case "email":
			return JStr(u.email)
		}
		return JNull()
	case "Repository":
		repo := obj.repo
		switch sel.Name {
		case "id":
			return JStr("R" + strconv.Itoa(repo.ID))
		case "databaseId":
			return JNum(int64(repo.ID))
		case "name":
			return JStr(repo.Name)
		case "nameWithOwner":
			return JStr(repo.Owner + "/" + repo.Name)
		case "owner":
			u := &gqlUser{login: repo.Owner, id: e.store.ViewerID, name: e.store.ViewerName, email: e.store.ViewerEmail}
			return e.resolveSelections(&gqlObj{typ: "User", user: u}, sel.Selections, vars)
		case "defaultBranchRef":
			return e.resolveSelections(&gqlObj{typ: "Ref", name: repo.DefaultBranch}, sel.Selections, vars)
		case "viewerPermission":
			return JStr("ADMIN")
		case "hasIssuesEnabled":
			return JBool(true)
		case "issue":
			issue := repo.Issues[gqlToInt(args["number"])]
			if issue == nil {
				return JNull()
			}
			return e.resolveSelections(&gqlObj{typ: "Issue", repo: repo, issue: issue}, sel.Selections, vars)
		case "issues":
			return e.resolveIssueConnection(repo, args, sel, vars)
		case "pullRequests":
			first := gqlToIntDef(args["first"], 100)
			pulls := make([]*Pull, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				pulls = append(pulls, p)
			}
			sortPullsBySeqDesc(pulls)
			if len(pulls) > first {
				pulls = pulls[:first]
			}
			var nodesSel []*Selection
			for _, s := range sel.Selections {
				if s.Name == "nodes" {
					nodesSel = s.Selections
				}
			}
			nodes := []*JVal{}
			for _, p := range pulls {
				nodes = append(nodes, e.resolveSelections(&gqlObj{typ: "PullRequest", repo: repo, pull: p}, nodesSel, vars))
			}
			return e.resolveConnectionNodes(sel, nodes, vars)
		case "pullRequest":
			pull := repo.Pulls[gqlToInt(args["number"])]
			if pull == nil {
				return JNull()
			}
			return e.resolveSelections(&gqlObj{typ: "PullRequest", repo: repo, pull: pull}, sel.Selections, vars)
		}
		return JNull()
	case "Issue":
		issue := obj.issue
		repo := obj.repo
		switch sel.Name {
		case "id":
			return JStr("I" + strconv.Itoa(repo.ID) + "_" + strconv.Itoa(issue.Number))
		case "number":
			return JNum(int64(issue.Number))
		case "title":
			return JStr(issue.Title)
		case "state":
			if issue.State == "open" {
				return JStr("OPEN")
			}
			return JStr("CLOSED")
		case "body":
			return JStr(issue.Body)
		}
		return JNull()
	case "CommentEdge":
		if sel.Name == "node" {
			c := obj.comment
			return e.resolveSelections(&gqlObj{typ: "Comment", comment: c, repo: obj.repo}, sel.Selections, vars)
		}
		return JNull()
	case "Comment":
		c := obj.comment
		repo := obj.repo
		if sel.Name == "id" {
			return JStr("C" + strconv.Itoa(repo.ID) + "_" + strconv.Itoa(c.ID))
		}
		if sel.Name == "body" {
			return JStr(c.Body)
		}
		return JNull()
	case "PullRequest":
		pull := obj.pull
		repo := obj.repo
		switch sel.Name {
		case "id":
			return JStr("P" + strconv.Itoa(repo.ID) + "_" + strconv.Itoa(pull.Number))
		case "number":
			return JNum(int64(pull.Number))
		case "title":
			return JStr(pull.Title)
		case "body":
			return JStr(pull.Body)
		case "state":
			if pull.Merged {
				return JStr("MERGED")
			}
			if pull.State == "open" {
				return JStr("OPEN")
			}
			return JStr("CLOSED")
		case "merged":
			return JBool(pull.Merged)
		case "headRefName":
			return JStr(pull.HeadRef)
		case "baseRefName":
			return JStr(pull.BaseRef)
		case "mergeable":
			if pull.Merged {
				return JStr("MERGEABLE")
			}
			headSha, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
			baseSha, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
			if !hasHead || !hasBase {
				return JStr("UNKNOWN")
			}
			if e.store.ComputePull(repo, headSha, baseSha).Conflict {
				return JStr("CONFLICTING")
			}
			return JStr("MERGEABLE")
		case "files":
			first := gqlToIntDef(args["first"], 100)
			files := pull.Files
			if len(files) > first {
				files = files[:first]
			}
			var nodesSel []*Selection
			if len(sel.Selections) > 0 {
				nodesSel = sel.Selections[0].Selections
			}
			if nodesSel == nil {
				nodesSel = []*Selection{{Name: "path"}}
			}
			nodes := []*JVal{}
			for _, f := range files {
				fields := []JPair{}
				for _, s := range nodesSel {
					key := s.Name
					if s.HasAlias {
						key = s.Alias
					}
					if s.Name == "path" {
						fields = append(fields, JField(key, JStr(f.Filename)))
					} else {
						fields = append(fields, JField(key, JNull()))
					}
				}
				nodes = append(nodes, JObj(fields...))
			}
			return e.resolveConnectionNodes(sel, nodes, vars)
		}
		return JNull()
	case "Ref":
		if sel.Name == "name" {
			return JStr(obj.name)
		}
		return JNull()
	case "__Type":
		name := obj.name
		switch sel.Name {
		case "name":
			return JStr(name)
		case "fields":
			fields := gqlTypeFields[name]
			items := []*JVal{}
			for _, f := range fields {
				items = append(items, JObj(JField("name", f)))
			}
			return JArr(items...)
		case "enumValues":
			if name == "SearchType" {
				items := []*JVal{}
				for _, v := range gqlSearchTypeValues {
					items = append(items, JObj(JField("name", v)))
				}
				return JArr(items...)
			}
			return JArr()
		}
		return JNull()
	}
	return JNull()
}

func (e *gqlExecutor) resolveIssueConnection(repo *Repo, args map[string]any, sel *Selection, vars map[string]any) *JVal {
	var states []string
	if arr, ok := args["states"].([]any); ok {
		for _, s := range arr {
			states = append(states, strings.ToLower(gqlToString(s)))
		}
	}
	orderBy := gqlToMap(args["orderBy"])
	direction := "DESC"
	if gqlToString(orderBy["direction"]) == "ASC" {
		direction = "ASC"
	}
	afterNum, hasAfter := 0, false
	if s, ok := args["after"].(string); ok {
		if n, ok := gqlNumberFromCursor(s); ok {
			afterNum, hasAfter = n, true
		}
	}
	list := []*Issue{}
	for _, i := range repo.Issues {
		list = append(list, i)
	}
	if states != nil {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[s] = true
		}
		filtered := []*Issue{}
		for _, i := range list {
			if wanted[i.State] {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	sortIssuesBySeq(list, direction == "ASC")
	if hasAfter {
		filtered := []*Issue{}
		for _, i := range list {
			if direction == "ASC" {
				if i.Number > afterNum {
					filtered = append(filtered, i)
				}
			} else {
				if i.Number < afterNum {
					filtered = append(filtered, i)
				}
			}
		}
		list = filtered
	}
	first := len(list)
	if n, ok := gqlToIntOk(args["first"]); ok {
		first = n
	}
	page := list
	if len(page) > first {
		page = page[:first]
	}
	hasNext := len(list) > first
	var nodesSel []*Selection
	var pageInfoSel *Selection
	for _, s := range sel.Selections {
		if s.Name == "nodes" {
			nodesSel = s.Selections
		}
		if s.Name == "pageInfo" {
			pageInfoSel = s
		}
	}
	nodes := []*JVal{}
	for _, issue := range page {
		nodes = append(nodes, e.resolveSelections(&gqlObj{typ: "Issue", repo: repo, issue: issue}, nodesSel, vars))
	}
	var endCursor *JVal = JNull()
	if len(page) > 0 {
		endCursor = JStr(gqlCursorForNumber(page[len(page)-1].Number))
	}
	pageInfoFields := []JPair{}
	if pageInfoSel != nil {
		for _, s := range pageInfoSel.Selections {
			key := s.Name
			if s.HasAlias {
				key = s.Alias
			}
			if s.Name == "hasNextPage" {
				pageInfoFields = append(pageInfoFields, JField(key, JBool(hasNext)))
			} else if s.Name == "endCursor" {
				pageInfoFields = append(pageInfoFields, JField(key, endCursor))
			}
		}
	}
	return JObj(JField("nodes", JArr(nodes...)), JField("pageInfo", JObj(pageInfoFields...)))
}

// resolveConnectionNodes is the generic connection wrapper for
// pullRequests/files: {nodes} (+pageInfo when requested).
func (e *gqlExecutor) resolveConnectionNodes(sel *Selection, nodes []*JVal, vars map[string]any) *JVal {
	if sel.Selections == nil {
		return JObj(JField("nodes", JArr(nodes...)))
	}
	fields := []JPair{}
	for _, s := range sel.Selections {
		key := s.Name
		if s.HasAlias {
			key = s.Alias
		}
		if s.Name == "nodes" {
			fields = append(fields, JField(key, JArr(nodes...)))
		} else if s.Name == "pageInfo" {
			fields = append(fields, JField(key, JObj(JField("hasNextPage", JBool(false)), JField("endCursor", JNull()))))
		} else {
			fields = append(fields, JField(key, JNull()))
		}
	}
	return JObj(fields...)
}

// ---------------------------------------------------------------- entry

// executeGraphQL runs one GraphQL request body (single operation or batch).
func executeGraphQL(store *Store, body any) *JVal {
	runOne := func(doc map[string]any) *JVal {
		if doc == nil {
			return JObj(JField("data", JNull()), JField("errors", JArr(JObj(JField("message", "query is required")))))
		}
		query, ok := doc["query"].(string)
		if !ok {
			return JObj(JField("data", JNull()), JField("errors", JArr(JObj(JField("message", "query is required")))))
		}
		tokens, err := gqlTokenize(query)
		if err != nil {
			return JObj(JField("data", JNull()), JField("errors", JArr(JObj(JField("message", err.Error())))))
		}
		p := &gqlParser{tokens: tokens, fragments: map[string]*gqlFragment{}}
		ops, err := p.parseDocument()
		if err != nil {
			return JObj(JField("data", JNull()), JField("errors", JArr(JObj(JField("message", err.Error())))))
		}
		if len(ops) == 0 {
			return JObj(JField("data", JObj()), JField("errors", JArr(JObj(JField("message", "empty document")))))
		}
		vars := gqlToMap(doc["variables"])
		op := ops[0]
		if name, ok := doc["operationName"].(string); ok {
			for _, o := range ops {
				if o.HasName && o.Name == name {
					op = o
					break
				}
			}
		}
		exec := &gqlExecutor{store: store}
		return exec.executeOperation(op, vars)
	}
	if arr, ok := body.([]any); ok {
		items := []*JVal{}
		for _, d := range arr {
			doc, _ := d.(map[string]any)
			items = append(items, runOne(doc))
		}
		return JArr(items...)
	}
	doc, _ := body.(map[string]any)
	return runOne(doc)
}

// ---------------------------------------------------------------- helpers

func gqlToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func gqlToMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func gqlToInt(v any) int {
	n, _ := gqlToIntOk(v)
	return n
}

func gqlToIntOk(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int64:
		return int(n), true
	case int:
		return n, true
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	}
	return 0, false
}

func gqlToIntDef(v any, def int) int {
	if n, ok := gqlToIntOk(v); ok {
		return n
	}
	return def
}

func sortPullsBySeqDesc(pulls []*Pull) {
	// Insertion sort is fine (small n); deterministic by seq desc.
	for i := 1; i < len(pulls); i++ {
		for j := i; j > 0 && pulls[j].Seq > pulls[j-1].Seq; j-- {
			pulls[j], pulls[j-1] = pulls[j-1], pulls[j]
		}
	}
}

func sortIssuesBySeq(list []*Issue, asc bool) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			if asc {
				if list[j].Seq >= list[j-1].Seq {
					break
				}
			} else {
				if list[j].Seq <= list[j-1].Seq {
					break
				}
			}
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
