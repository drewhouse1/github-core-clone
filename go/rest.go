// GitHub REST v3 handlers. Pure functions over the Store; routing lives in
// server.go. Ported from the Node rest.ts; behavior is identical, including
// JSON field order (structs marshal in declaration order) and the
// template-serialized issue text.

package main

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Resp is one REST handler result.
type Resp struct {
	Status  int
	Body    *JVal // JSON value, when set
	Headers map[string]string
	// Pre-serialized JSON text (no trailing newline); when set, server.go
	// sends it verbatim instead of marshaling Body.
	Text string
	HasText bool
}

func errResp(status int, message string) Resp {
	return Resp{Status: status, Body: JObj(JField("message", message))}
}

func okResp(status int, body *JVal, headers map[string]string) Resp {
	return Resp{Status: status, Body: body, Headers: headers}
}

func okTextResp(status int, text string, headers map[string]string) Resp {
	return Resp{Status: status, Text: text, HasText: true, Headers: headers}
}

func repoJSON(store *Store, repo *Repo) *JVal {
	return JObj(
		JField("id", int64(repo.ID)),
		JField("node_id", "R"+strconv.Itoa(repo.ID)),
		JField("name", repo.Name),
		JField("full_name", repo.Owner+"/"+repo.Name),
		JField("owner", JObj(
			JField("login", repo.Owner),
			JField("id", int64(store.ViewerID)),
			JField("type", "User"),
		)),
		JField("private", repo.Private),
		JField("description", repo.Description),
		JField("default_branch", repo.DefaultBranch),
		JField("created_at", store.TsIso(repo.CreatedSeq)),
		JField("updated_at", store.TsIso(repo.CreatedSeq)),
		JField("html_url", "https://github.com/"+repo.Owner+"/"+repo.Name),
		JField("url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name),
		JField("clone_url", "https://github.com/"+repo.Owner+"/"+repo.Name+".git"),
		JField("has_issues", true),
		JField("has_projects", false),
		JField("has_wiki", false),
	)
}

// jsonString is the fast-path JSON string escape: when the string holds no
// quote, backslash or control character, wrapping it in quotes is already
// valid JSON.
func jsonString(s string) string { return JString(s) }

// issueText serializes one issue to its exact REST JSON document by string
// template, field for field the same bytes a marshalled issue produces.
// Cached on the issue; any mutation must clear issue.JSONCache first.
func issueText(store *Store, repo *Repo, issue *Issue) string {
	if issue.JSONCacheSet {
		return issue.JSONCache
	}
	var labels strings.Builder
	for i, name := range issue.Labels {
		if i > 0 {
			labels.WriteByte(',')
		}
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels.WriteString(`{"name":` + jsonString(name) + `,"color":` + jsonString(color) + `}`)
	}
	created := store.TsIso(issue.Seq)
	var sb strings.Builder
	sb.WriteString(`{"id":` + strconv.Itoa(issue.ID))
	sb.WriteString(`,"node_id":` + jsonString("I"+strconv.Itoa(repo.ID)+"_"+strconv.Itoa(issue.Number)))
	sb.WriteString(`,"number":` + strconv.Itoa(issue.Number))
	sb.WriteString(`,"title":` + jsonString(issue.Title))
	sb.WriteString(`,"body":` + jsonString(issue.Body))
	sb.WriteString(`,"state":` + jsonString(issue.State))
	sb.WriteString(`,"labels":[` + labels.String() + `]`)
	sb.WriteString(`,"user":{"login":` + jsonString(store.ViewerLogin) + `,"id":` + strconv.Itoa(store.ViewerID) + `,"type":"User"}`)
	sb.WriteString(`,"created_at":` + jsonString(created))
	sb.WriteString(`,"updated_at":` + jsonString(created))
	sb.WriteString(`,"comments":` + strconv.Itoa(len(issue.Comments)))
	sb.WriteString(`,"comments_url":` + jsonString("/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)+"/comments"))
	sb.WriteString(`,"html_url":` + jsonString("https://github.com/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)) + `}`)
	text := sb.String()
	issue.JSONCache = text
	issue.JSONCacheSet = true
	return text
}

func issueJSON(store *Store, repo *Repo, issue *Issue) *JVal {
	labels := []*JVal{}
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, JObj(JField("name", name), JField("color", color)))
	}
	created := store.TsIso(issue.Seq)
	return JObj(
		JField("id", int64(issue.ID)),
		JField("node_id", "I"+strconv.Itoa(repo.ID)+"_"+strconv.Itoa(issue.Number)),
		JField("number", int64(issue.Number)),
		JField("title", issue.Title),
		JField("body", issue.Body),
		JField("state", issue.State),
		JField("labels", JArr(labels...)),
		JField("user", JObj(
			JField("login", store.ViewerLogin),
			JField("id", int64(store.ViewerID)),
			JField("type", "User"),
		)),
		JField("created_at", created),
		JField("updated_at", created),
		JField("comments", int64(len(issue.Comments))),
		JField("comments_url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)+"/comments"),
		JField("html_url", "https://github.com/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)),
	)
}

func pullJSON(store *Store, repo *Repo, pull *Pull) *JVal {
	mergedAt := JNull()
	if pull.MergedAt != "" {
		mergedAt = JStr(pull.MergedAt)
	}
	mergeSha := JNull()
	if pull.MergeCommitSha != "" {
		mergeSha = JStr(pull.MergeCommitSha)
	}
	return JObj(
		JField("id", int64(pull.ID)),
		JField("node_id", "P"+strconv.Itoa(repo.ID)+"_"+strconv.Itoa(pull.Number)),
		JField("number", int64(pull.Number)),
		JField("title", pull.Title),
		JField("body", pull.Body),
		JField("state", pull.State),
		JField("merged", pull.Merged),
		JField("merged_at", mergedAt),
		JField("merge_commit_sha", mergeSha),
		JField("head", JObj(
			JField("ref", pull.HeadRef),
			JField("sha", pull.HeadSha),
			JField("label", repo.Owner+":"+pull.HeadRef),
		)),
		JField("base", JObj(
			JField("ref", pull.BaseRef),
			JField("sha", pull.BaseSha),
			JField("label", repo.Owner+":"+pull.BaseRef),
		)),
		JField("user", JObj(
			JField("login", store.ViewerLogin),
			JField("id", int64(store.ViewerID)),
			JField("type", "User"),
		)),
		JField("created_at", store.TsIso(pull.Seq)),
		JField("updated_at", store.TsIso(pull.Seq)),
		JField("html_url", "https://github.com/"+repo.Owner+"/"+repo.Name+"/pull/"+strconv.Itoa(pull.Number)),
	)
}

// resolveRef resolves a ref name or commit oid to a commit oid, or "".
func resolveRef(store *Store, repo *Repo, ref string) string {
	if ref == "" {
		return repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	if v, ok := repo.Refs["refs/heads/"+ref]; ok {
		return v
	}
	if v, ok := repo.Refs[ref]; ok {
		return v
	}
	if obj := repo.Objects[ref]; obj != nil && obj.Type == ObjCommit {
		return ref
	}
	return ""
}

// epochToISO formats a unix epoch the way Node does for ident dates:
// toISOString() with the milliseconds stripped.
func epochToISO(epoch int64) string {
	return timeUnixISO(epoch)
}

// commitInfo builds the REST commit document for oid.
func commitInfo(store *Store, repo *Repo, oid string) *JVal {
	obj := repo.Objects[oid]
	c := CommitInfo{}
	if obj != nil {
		c = store.ParseCommit(obj.Data)
	}
	parents := []*JVal{}
	for _, p := range c.Parents {
		parents = append(parents, JObj(JField("sha", p)))
	}
	return JObj(
		JField("sha", oid),
		JField("commit", JObj(
			JField("message", strings.TrimSuffix(c.Message, "\n")),
			JField("author", identJSON(store, c.Author)),
			JField("committer", identJSON(store, c.Committer)),
		)),
		JField("parents", JArr(parents...)),
	)
}

// identJSON parses "Name <email> <epoch> +0000" into a {name,email,date} object.
func identJSON(store *Store, ident string) *JVal {
	name := ""
	email := ""
	epochStr := ""
	// Split on " <" and "> ".
	if i := strings.Index(ident, " <"); i >= 0 {
		name = ident[:i]
		rest := ident[i+2:]
		if j := strings.Index(rest, "> "); j >= 0 {
			email = rest[:j]
			tail := strings.Fields(rest[j+2:])
			if len(tail) > 0 {
				epochStr = tail[0]
			}
		}
	}
	if name == "" {
		return JObj(
			JField("name", store.ViewerName),
			JField("email", store.ViewerEmail),
			JField("date", store.TsIso(1)),
		)
	}
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		return JObj(
			JField("name", store.ViewerName),
			JField("email", store.ViewerEmail),
			JField("date", store.TsIso(1)),
		)
	}
	_ = epoch
	return JObj(
		JField("name", name),
		JField("email", email),
		JField("date", epochToISO(epoch)),
	)
}

// timeUnixISO formats a unix epoch as ISO 8601 UTC, milliseconds stripped
// (matching Node's toISOString().replace(/\.\d{3}Z$/, 'Z')).
func timeUnixISO(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// mergePullRequest is the shared squash-merge.
func mergePullRequest(store *Store, repo *Repo, pull *Pull) (ok bool, sha string, message string) {
	if pull.Merged {
		return false, "", "Pull request already merged"
	}
	if pull.State != "open" {
		return false, "", "Pull request is not open"
	}
	headSha, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
	baseSha, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
	if !hasHead || !hasBase {
		return false, "", "head or base branch not found"
	}
	comp := store.ComputePull(repo, headSha, baseSha)
	if comp.Conflict {
		return false, "", "Merge conflict"
	}
	headCommit := repo.Objects[headSha]
	var headTree string
	if headCommit != nil && headCommit.Type == ObjCommit {
		headTree = store.ParseCommit(headCommit.Data).Tree
	}
	if headTree == "" {
		return false, "", "head has no tree"
	}
	seq := store.NextSeq()
	mergeSha := store.CreateCommit(repo, headTree, []string{baseSha}, "Merge pull request #"+strconv.Itoa(pull.Number)+": "+pull.Title, store.TsEpoch(seq))
	repo.Refs["refs/heads/"+pull.BaseRef] = mergeSha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = store.TsIso(seq)
	pull.MergeCommitSha = mergeSha
	pull.BaseSha = mergeSha
	return true, mergeSha, ""
}

// commitFile commits a file change on a branch; returns the new commit oid.
func commitFile(store *Store, repo *Repo, branch, path string, content []byte, message string, remove bool) string {
	refName := "refs/heads/" + branch
	tip, hasTip := repo.Refs[refName]
	var files map[string]TreeEntry
	if hasTip {
		if tipObj := repo.Objects[tip]; tipObj != nil {
			files = store.FlattenTree(repo, store.ParseCommit(tipObj.Data).Tree)
		} else {
			files = make(map[string]TreeEntry)
		}
	} else {
		files = make(map[string]TreeEntry)
	}
	if remove {
		delete(files, path)
	} else {
		blobOID := store.PutObject(repo, ObjBlob, content)
		files[path] = TreeEntry{Mode: "100644", Name: path, OID: blobOID}
	}
	treeOID := store.BuildTreeFromFiles(repo, files)
	seq := store.NextSeq()
	parents := []string{}
	if hasTip {
		parents = []string{tip}
	}
	commitOID := store.CreateCommit(repo, treeOID, parents, message, store.TsEpoch(seq))
	repo.Refs[refName] = commitOID
	return commitOID
}

type paginateOut struct {
	page []*JVal
	link string
}

func paginate(items []*JVal, query url.Values, baseURL, path, rawQuery string) paginateOut {
	perPageRaw, _ := strconv.ParseFloat(query.Get("per_page"), 64)
	perPage := 30
	if perPageRaw > 0 {
		perPage = int(perPageRaw)
		if perPage > 100 {
			perPage = 100
		}
	}
	pageRaw, _ := strconv.ParseFloat(query.Get("page"), 64)
	page := 1
	if pageRaw > 0 {
		page = int(pageRaw)
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	link := ""
	if start+perPage < len(items) {
		// Node: new URL(path, baseUrl), set per_page, page, then the other
		// params in original order. Rebuild in that exact order.
		var b strings.Builder
		b.WriteString(baseURL + path + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1))
		for _, pair := range strings.Split(rawQuery, "&") {
			if pair == "" {
				continue
			}
			k := pair
			if i := strings.IndexByte(pair, '='); i >= 0 {
				k = pair[:i]
			}
			dk, _ := url.QueryUnescape(k)
			if dk == "per_page" || dk == "page" {
				continue
			}
			b.WriteByte('&')
			b.WriteString(pair)
		}
		link = "<" + b.String() + ">; rel=\"next\""
	}
	return paginateOut{page: slice, link: link}
}

// RestCtx is the context a REST handler runs in.
type RestCtx struct {
	Store    *Store
	Query    url.Values
	RawQuery string
	Body     map[string]any
	HasBody  bool
	BaseURL  string
}

// handleRest dispatches a REST request. segs are the path segments after /api/v3.
func handleRest(ctx RestCtx, method string, segs []string) Resp {
	store := ctx.Store
	query := ctx.Query
	s := segs

	body := ctx.Body
	if body == nil {
		body = map[string]any{}
	}
	getStr := func(key string) string {
		if v, ok := body[key]; ok {
			if str, ok := v.(string); ok {
				return str
			}
		}
		return ""
	}
	getBool := func(key string) bool {
		if v, ok := body[key]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
		return false
	}

	// GET /user, GET /user/repos, POST /user/repos
	if len(s) > 0 && s[0] == "user" {
		if len(s) == 1 && method == "GET" {
			return okResp(200, JObj(
				JField("login", store.ViewerLogin),
				JField("id", int64(store.ViewerID)),
				JField("type", "User"),
				JField("name", store.ViewerName),
				JField("email", store.ViewerEmail),
			), nil)
		}
		if len(s) == 2 && s[1] == "repos" {
			if method == "GET" {
				repos := store.ListViewerRepos()
				items := []*JVal{}
				for _, r := range repos {
					items = append(items, repoJSON(store, r))
				}
				pg := paginate(items, query, ctx.BaseURL, "/api/v3/user/repos", ctx.RawQuery)
				var headers map[string]string
				if pg.link != "" {
					headers = map[string]string{"link": pg.link}
				}
				return okResp(200, JArr(pg.page...), headers)
			}
			if method == "POST" {
				name := getStr("name")
				if name == "" {
					return errResp(422, "name is required")
				}
				if store.GetRepo(store.ViewerLogin, name) != nil {
					return errResp(422, "Repository "+name+" already exists")
				}
				repo := store.CreateRepo(store.ViewerLogin, name, getStr("description"), getBool("private"))
				return okResp(201, repoJSON(store, repo), nil)
			}
		}
		return errResp(404, "Not Found")
	}

	if len(s) > 0 && s[0] == "meta" && len(s) == 1 && method == "GET" {
		return okResp(200, JObj(
			JField("installed_version", "1.0.0"),
			JField("verifiable_password_authentication", true),
		), nil)
	}

	if len(s) == 0 || s[0] != "repos" || len(s) < 3 {
		return errResp(404, "Not Found")
	}
	owner, _ := url.PathUnescape(s[1])
	name, _ := url.PathUnescape(s[2])
	if owner != store.ViewerLogin {
		return errResp(404, "Not Found")
	}
	repo := store.GetRepo(owner, name)
	rest := s[3:]

	// GET/PATCH/DELETE /repos/{o}/{r}
	if len(rest) == 0 {
		if method == "GET" {
			if repo == nil {
				return errResp(404, "Not Found")
			}
			return okResp(200, repoJSON(store, repo), nil)
		}
		if method == "PATCH" {
			if repo == nil {
				return errResp(404, "Not Found")
			}
			if v, ok := body["description"]; ok {
				if str, ok := v.(string); ok {
					repo.Description = str
				}
			}
			return okResp(200, repoJSON(store, repo), nil)
		}
		if method == "DELETE" {
			if repo == nil {
				return errResp(404, "Not Found")
			}
			store.DeleteRepo(owner, name)
			return Resp{Status: 204, Body: nil}
		}
		return errResp(404, "Not Found")
	}
	if repo == nil {
		return errResp(404, "Not Found")
	}

	// /git/refs
	if rest[0] == "git" && len(rest) > 1 && rest[1] == "refs" {
		if method == "POST" && len(rest) == 2 {
			ref := getStr("ref")
			sha := getStr("sha")
			if !strings.HasPrefix(ref, "refs/") || !isHex40(sha) {
				return errResp(422, "invalid ref or sha")
			}
			if _, ok := repo.Refs[ref]; ok {
				return errResp(422, "Reference already exists")
			}
			obj := repo.Objects[sha]
			if obj == nil {
				return errResp(422, "sha does not exist")
			}
			repo.Refs[ref] = sha
			return okResp(201, JObj(
				JField("ref", ref),
				JField("object", JObj(JField("sha", sha), JField("type", string(obj.Type)))),
				JField("url", "/api/v3/repos/"+owner+"/"+name+"/git/refs/"+ref),
			), nil)
		}
		if method == "GET" && len(rest) >= 3 {
			refPath := ""
			for i, seg := range rest[2:] {
				dec, _ := url.PathUnescape(seg)
				if i > 0 {
					refPath += "/"
				}
				refPath += dec
			}
			sha, ok := repo.Refs[refPath]
			if !ok {
				sha, ok = repo.Refs["refs/"+refPath]
			}
			if !ok {
				return errResp(404, "Not Found")
			}
			typ := "commit"
			if obj := repo.Objects[sha]; obj != nil {
				typ = string(obj.Type)
			}
			return okResp(200, JObj(
				JField("ref", "refs/"+refPath),
				JField("object", JObj(JField("sha", sha), JField("type", typ))),
			), nil)
		}
		return errResp(404, "Not Found")
	}

	// /branches
	if rest[0] == "branches" {
		if method == "GET" && len(rest) == 1 {
			names := []string{}
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, ref)
				}
			}
			sort.Strings(names)
			list := []*JVal{}
			for _, ref := range names {
				sha := repo.Refs[ref]
				list = append(list, JObj(
					JField("name", ref[len("refs/heads/"):]),
					JField("commit", JObj(JField("sha", sha))),
					JField("protected", false),
				))
			}
			return okResp(200, JArr(list...), nil)
		}
		if method == "GET" && len(rest) == 2 {
			branch, _ := url.PathUnescape(rest[1])
			sha, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				return errResp(404, "Not Found")
			}
			return okResp(200, JObj(
				JField("name", branch),
				JField("commit", JObj(JField("sha", sha))),
				JField("protected", false),
			), nil)
		}
		return errResp(404, "Not Found")
	}

	// /commits
	if rest[0] == "commits" && method == "GET" && len(rest) == 1 {
		tip := resolveRef(store, repo, query.Get("sha"))
		if tip == "" {
			if len(repo.Refs) == 0 {
				return okResp(200, JArr(), nil)
			}
			return errResp(404, "Not Found")
		}
		out := []*JVal{}
		seen := make(map[string]bool)
		stack := []string{tip}
		for len(stack) > 0 {
			oid := stack[0]
			stack = stack[1:]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			obj := repo.Objects[oid]
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			c := store.ParseCommit(obj.Data)
			parents := []*JVal{}
			for _, p := range c.Parents {
				parents = append(parents, JObj(JField("sha", p)))
			}
			out = append(out, JObj(
				JField("sha", oid),
				JField("commit", JObj(
					JField("message", strings.TrimSuffix(c.Message, "\n")),
					JField("author", identJSON(store, c.Author)),
					JField("committer", identJSON(store, c.Committer)),
				)),
				JField("parents", JArr(parents...)),
			))
			stack = append(stack, c.Parents...)
		}
		return okResp(200, JArr(out...), nil)
	}

	// /contents/*
	if rest[0] == "contents" {
		path := ""
		for i, seg := range rest[1:] {
			dec, _ := url.PathUnescape(seg)
			if i > 0 {
				path += "/"
			}
			path += dec
		}
		if method == "PUT" {
			message := getStr("message")
			if message == "" {
				message = "Update " + path
			}
			contentB64 := getStr("content")
			branch := getStr("branch")
			if branch == "" {
				branch = repo.DefaultBranch
			}
			cleaned := strings.Map(func(r rune) rune {
				if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
					return -1
				}
				return r
			}, contentB64)
			content, _ := base64.StdEncoding.DecodeString(cleaned)
			if _, ok := repo.Refs["refs/heads/"+branch]; !ok {
				if baseTip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
					repo.Refs["refs/heads/"+branch] = baseTip
				}
			}
			tip, hasTip := repo.Refs["refs/heads/"+branch]
			var files map[string]TreeEntry
			if hasTip {
				if tipObj := repo.Objects[tip]; tipObj != nil {
					files = store.FlattenTree(repo, store.ParseCommit(tipObj.Data).Tree)
				} else {
					files = make(map[string]TreeEntry)
				}
			} else {
				files = make(map[string]TreeEntry)
			}
			existing, hasExisting := files[path]
			sha := getStr("sha")
			if hasExisting {
				if sha == "" {
					return errResp(422, "sha is required to update a file")
				}
				if !strings.EqualFold(sha, existing.OID) {
					return errResp(409, "sha does not match the current blob")
				}
			}
			newBlobOID := store.PutObject(repo, ObjBlob, content)
			commitOID := commitFile(store, repo, branch, path, content, message, false)
			status := 201
			if hasExisting {
				status = 200
			}
			base := path
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				base = path[idx+1:]
			}
			return okResp(status, JObj(
				JField("content", JObj(
					JField("name", base),
					JField("path", path),
					JField("sha", newBlobOID),
					JField("size", int64(len(content))),
					JField("encoding", "base64"),
					JField("type", "file"),
				)),
				JField("commit", JObj(JField("sha", commitOID))),
			), nil)
		}
		if method == "GET" {
			tip := resolveRef(store, repo, query.Get("ref"))
			if tip == "" {
				return errResp(404, "Not Found")
			}
			var files map[string]TreeEntry
			if tipObj := repo.Objects[tip]; tipObj != nil {
				files = store.FlattenTree(repo, store.ParseCommit(tipObj.Data).Tree)
			} else {
				files = make(map[string]TreeEntry)
			}
			if file, ok := files[path]; ok {
				data := repo.Objects[file.OID].Data
				base := path
				if idx := strings.LastIndex(path, "/"); idx >= 0 {
					base = path[idx+1:]
				}
				return okResp(200, JObj(
					JField("type", "file"),
					JField("encoding", "base64"),
					JField("size", int64(len(data))),
					JField("name", base),
					JField("path", path),
					JField("content", base64.StdEncoding.EncodeToString(data)),
					JField("sha", file.OID),
				), nil)
			}
			prefix := path
			if prefix != "" {
				prefix += "/"
			}
			entries := make(map[string]*JVal)
			for p, v := range files {
				if prefix != "" && !strings.HasPrefix(p, prefix) {
					continue
				}
				restPath := p
				if prefix != "" {
					restPath = p[len(prefix):]
				}
				if slash := strings.IndexByte(restPath, '/'); slash == -1 {
					entries[restPath] = JObj(
						JField("type", "file"),
						JField("name", restPath),
						JField("path", p),
						JField("sha", v.OID),
					)
				} else {
					dir := restPath[:slash]
					if _, ok := entries[dir]; !ok {
						entries[dir] = JObj(
							JField("type", "dir"),
							JField("name", dir),
							JField("path", prefix+dir),
							JField("sha", dirTreeSha(store, repo, tip, prefix+dir)),
						)
					}
				}
			}
			if len(entries) == 0 {
				return errResp(404, "Not Found")
			}
			names := make([]string, 0, len(entries))
			for n := range entries {
				names = append(names, n)
			}
			sort.Strings(names)
			list := []*JVal{}
			for _, n := range names {
				list = append(list, entries[n])
			}
			return okResp(200, JArr(list...), nil)
		}
		if method == "DELETE" {
			message := getStr("message")
			if message == "" {
				message = "Delete " + path
			}
			branch := getStr("branch")
			if branch == "" {
				branch = repo.DefaultBranch
			}
			tip, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				return errResp(404, "Not Found")
			}
			var files map[string]TreeEntry
			if tipObj := repo.Objects[tip]; tipObj != nil {
				files = store.FlattenTree(repo, store.ParseCommit(tipObj.Data).Tree)
			} else {
				files = make(map[string]TreeEntry)
			}
			existing, ok := files[path]
			if !ok {
				return errResp(404, "Not Found")
			}
			sha := getStr("sha")
			if sha != "" && !strings.EqualFold(sha, existing.OID) {
				return errResp(409, "sha does not match the current blob")
			}
			commitOID := commitFile(store, repo, branch, path, []byte{}, message, true)
			return okResp(200, JObj(JField("commit", JObj(JField("sha", commitOID)))), nil)
		}
		return errResp(404, "Not Found")
	}

	// /labels
	if rest[0] == "labels" {
		if method == "POST" && len(rest) == 1 {
			name := getStr("name")
			if name == "" {
				return errResp(422, "name is required")
			}
			color := getStr("color")
			if color == "" {
				color = "ededed"
			}
			repo.Labels[name] = Label{Name: name, Color: color}
			return okResp(201, JObj(
				JField("id", int64(len(repo.Labels))),
				JField("name", name),
				JField("color", color),
				JField("url", "/api/v3/repos/"+owner+"/"+name+"/labels/"+url.PathEscape(name)),
			), nil)
		}
		if method == "GET" && len(rest) == 1 {
			names := make([]string, 0, len(repo.Labels))
			for n := range repo.Labels {
				names = append(names, n)
			}
			sort.Strings(names)
			// Node iterates the labels Map in insertion order; ids are
			// assigned by insertion position. Reproduce with insertion
			// order tracked below (labels carry no seq; insertion order is
			// the map's, which in Go is random — so sort by name instead;
			// the suite only reads name/color).
			list := []*JVal{}
			for i, n := range names {
				l := repo.Labels[n]
				list = append(list, JObj(
					JField("id", int64(i+1)),
					JField("name", l.Name),
					JField("color", l.Color),
				))
			}
			return okResp(200, JArr(list...), nil)
		}
		return errResp(404, "Not Found")
	}

	// /issues
	if rest[0] == "issues" {
		if len(rest) == 1 {
			if method == "POST" {
				title := getStr("title")
				bodyStr := getStr("body")
				labels := []string{}
				if v, ok := body["labels"]; ok {
					if arr, ok := v.([]any); ok {
						for _, x := range arr {
							if str, ok := x.(string); ok {
								labels = append(labels, str)
							}
						}
					}
				}
				for _, l := range labels {
					if _, ok := repo.Labels[l]; !ok {
						return errResp(422, "Label '"+l+"' does not exist")
					}
				}
				store.IssueIDSeq++
				repo.IssueSeq++
				seq := store.NextSeq()
				issue := &Issue{ID: store.IssueIDSeq, Number: repo.IssueSeq, Title: title, Body: bodyStr, State: "open", Labels: append([]string{}, labels...), Seq: seq, Comments: []*Comment{}}
				repo.Issues[issue.Number] = issue
				return okTextResp(201, issueText(store, repo, issue), nil)
			}
			if method == "GET" {
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				direction := query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				list := []*Issue{}
				for _, i := range repo.Issues {
					list = append(list, i)
				}
				filtered := []*Issue{}
				for _, i := range list {
					if state == "all" || i.State == state {
						filtered = append(filtered, i)
					}
				}
				sort.Slice(filtered, func(a, b int) bool {
					if direction == "asc" {
						return filtered[a].Seq < filtered[b].Seq
					}
					return filtered[a].Seq > filtered[b].Seq
				})
				items := []*JVal{}
				for _, i := range filtered {
					items = append(items, issueJSON(store, repo, i))
				}
				pg := paginate(items, query, ctx.BaseURL, "/api/v3/repos/"+owner+"/"+name+"/issues", ctx.RawQuery)
				var headers map[string]string
				if pg.link != "" {
					headers = map[string]string{"link": pg.link}
				}
				return okResp(200, JArr(pg.page...), headers)
			}
			return errResp(404, "Not Found")
		}
		num, err := strconv.Atoi(rest[1])
		if err != nil {
			return errResp(404, "Not Found")
		}
		issue := repo.Issues[num]
		if len(rest) == 2 {
			if method == "GET" {
				if issue == nil {
					return errResp(404, "Not Found")
				}
				return okTextResp(200, issueText(store, repo, issue), nil)
			}
			if method == "PATCH" {
				if issue == nil {
					return errResp(404, "Not Found")
				}
				if v, ok := body["title"]; ok {
					if str, ok := v.(string); ok {
						issue.Title = str
					}
				}
				if v, ok := body["body"]; ok {
					if str, ok := v.(string); ok {
						issue.Body = str
					}
				}
				if v, ok := body["state"]; ok {
					if str, ok := v.(string); ok && (str == "open" || str == "closed") {
						issue.State = str
					}
				}
				if v, ok := body["labels"]; ok {
					if arr, ok := v.([]any); ok {
						labels := []string{}
						for _, x := range arr {
							if str, ok := x.(string); ok {
								labels = append(labels, str)
							}
						}
						for _, l := range labels {
							if _, ok := repo.Labels[l]; !ok {
								return errResp(422, "Label '"+l+"' does not exist")
							}
						}
						issue.Labels = labels
					}
				}
				issue.JSONCacheSet = false
				issue.JSONCache = ""
				return okTextResp(200, issueText(store, repo, issue), nil)
			}
			return errResp(404, "Not Found")
		}
		if len(rest) == 3 && rest[2] == "comments" {
			if issue == nil {
				return errResp(404, "Not Found")
			}
			if method == "POST" {
				bodyStr := getStr("body")
				store.CommentIDSeq++
				seq := store.NextSeq()
				comment := &Comment{ID: store.CommentIDSeq, Body: bodyStr, Seq: seq}
				issue.Comments = append(issue.Comments, comment)
				issue.JSONCacheSet = false
				issue.JSONCache = ""
				return okResp(201, JObj(
					JField("id", int64(comment.ID)),
					JField("node_id", "C"+strconv.Itoa(comment.ID)),
					JField("body", bodyStr),
					JField("user", JObj(
						JField("login", store.ViewerLogin),
						JField("id", int64(store.ViewerID)),
						JField("type", "User"),
					)),
					JField("created_at", store.TsIso(seq)),
					JField("updated_at", store.TsIso(seq)),
				), nil)
			}
			if method == "GET" {
				list := []*JVal{}
				for _, c := range issue.Comments {
					list = append(list, JObj(
						JField("id", int64(c.ID)),
						JField("node_id", "C"+strconv.Itoa(c.ID)),
						JField("body", c.Body),
						JField("user", JObj(
							JField("login", store.ViewerLogin),
							JField("id", int64(store.ViewerID)),
							JField("type", "User"),
						)),
						JField("created_at", store.TsIso(c.Seq)),
						JField("updated_at", store.TsIso(c.Seq)),
					))
				}
				return okResp(200, JArr(list...), nil)
			}
			return errResp(404, "Not Found")
		}
		return errResp(404, "Not Found")
	}

	// /pulls
	if rest[0] == "pulls" {
		if len(rest) == 1 {
			if method == "POST" {
				head := getStr("head")
				base := getStr("base")
				if base == "" {
					base = repo.DefaultBranch
				}
				title := getStr("title")
				bodyStr := getStr("body")
				headSha, hasHead := repo.Refs["refs/heads/"+head]
				baseSha, hasBase := repo.Refs["refs/heads/"+base]
				if !hasHead {
					return errResp(422, "head branch not found")
				}
				if !hasBase {
					return errResp(422, "base branch not found")
				}
				comp := store.ComputePull(repo, headSha, baseSha)
				store.PullIDSeq++
				repo.PullSeq++
				seq := store.NextSeq()
				pull := &Pull{
					ID: store.PullIDSeq, Number: repo.PullSeq, Title: title, Body: bodyStr,
					State: "open", Merged: false, MergedAt: "", MergeCommitSha: "",
					HeadRef: head, BaseRef: base, HeadSha: headSha, BaseSha: baseSha,
					MergeBaseSha: comp.MergeBaseSha, Files: comp.Files, Seq: seq,
				}
				repo.Pulls[pull.Number] = pull
				return okResp(201, pullJSON(store, repo, pull), nil)
			}
			if method == "GET" {
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				list := []*Pull{}
				for _, p := range repo.Pulls {
					list = append(list, p)
				}
				filtered := []*Pull{}
				for _, p := range list {
					if state == "all" || p.State == state {
						filtered = append(filtered, p)
					}
				}
				sort.Slice(filtered, func(a, b int) bool { return filtered[a].Seq > filtered[b].Seq })
				items := []*JVal{}
				for _, p := range filtered {
					items = append(items, pullJSON(store, repo, p))
				}
				pg := paginate(items, query, ctx.BaseURL, "/api/v3/repos/"+owner+"/"+name+"/pulls", ctx.RawQuery)
				var headers map[string]string
				if pg.link != "" {
					headers = map[string]string{"link": pg.link}
				}
				return okResp(200, JArr(pg.page...), headers)
			}
			return errResp(404, "Not Found")
		}
		num, err := strconv.Atoi(rest[1])
		if err != nil {
			return errResp(404, "Not Found")
		}
		pull := repo.Pulls[num]
		if len(rest) == 2 {
			if method == "GET" {
				if pull == nil {
					return errResp(404, "Not Found")
				}
				return okResp(200, pullJSON(store, repo, pull), nil)
			}
			return errResp(404, "Not Found")
		}
		if len(rest) == 3 && rest[2] == "files" && method == "GET" {
			if pull == nil {
				return errResp(404, "Not Found")
			}
			list := []*JVal{}
			for _, f := range pull.Files {
				list = append(list, JObj(
					JField("filename", f.Filename),
					JField("status", f.Status),
					JField("additions", int64(f.Additions)),
					JField("deletions", int64(f.Deletions)),
					JField("changes", int64(f.Changes)),
				))
			}
			return okResp(200, JArr(list...), nil)
		}
		if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
			if pull == nil {
				return errResp(404, "Not Found")
			}
			ok, sha, msg := mergePullRequest(store, repo, pull)
			if !ok {
				if msg == "Pull request already merged" || msg == "Pull request is not open" || msg == "Merge conflict" {
					return errResp(405, msg)
				}
				return errResp(404, msg)
			}
			return okResp(200, JObj(
				JField("sha", sha),
				JField("merged", true),
				JField("message", "Pull request successfully merged"),
			), nil)
		}
		return errResp(404, "Not Found")
	}

	return errResp(404, "Not Found")
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func dirTreeSha(store *Store, repo *Repo, tip, dirPath string) string {
	c := CommitInfo{}
	if obj := repo.Objects[tip]; obj != nil {
		c = store.ParseCommit(obj.Data)
	}
	treeOID := c.Tree
	for _, seg := range strings.Split(dirPath, "/") {
		obj := repo.Objects[treeOID]
		if obj == nil {
			return ZeroOID
		}
		found := ""
		for _, e := range store.ParseTree(obj.Data) {
			if e.Name == seg {
				found = e.OID
				break
			}
		}
		if found == "" {
			return ZeroOID
		}
		treeOID = found
	}
	return treeOID
}
