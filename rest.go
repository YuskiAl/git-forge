// REST v3 (api.github.com-compatible surface). Every reply is JSON with a
// non-empty X-GitHub-Request-Id; unknown routes are JSON 404s; malformed
// JSON bodies are 400s; error bodies carry a string `message`. Port of rest.ts.
//
// Locking: every function here assumes the caller holds the store write
// lock (the server takes it once per request); no function locks itself,
// so GraphQL/MCP can call in without deadlocking.

package main

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func isoStr(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// Result is one HTTP reply (before framing).
type Result struct {
	Status      int
	ContentType string // "" means no Content-Type header
	Body        []byte
	Extra       [][2]string // extra headers, in order
	NoBody      bool
}

func jsonResult(status int, body []byte, rid string, extra ...[2]string) Result {
	h := [][2]string{{"Content-Type", "application/json; charset=utf-8"}, {"X-GitHub-Request-Id", rid}}
	h = append(h, extra...)
	return Result{Status: status, ContentType: "", Body: body, Extra: h}
}

func errorResult(status int, message, rid string) Result {
	body := appendJSONString(nil, message)
	body = append([]byte(`{"message":`), body...)
	body = append(body, '}')
	return jsonResult(status, body, rid)
}

type RestCtx struct {
	Store     *Store
	RequestID string
	Host      string
}

func userJSON() []byte {
	return []byte(`{"login":"adaiversen","id":1000,"name":"ada iversen","email":"adaiversen@example.invalid","type":"User"}`)
}

func repoJSON(ctx RestCtx, repo *Repo) []byte {
	created := isoStr(epochSec + repo.CreatedSeq)
	open := 0
	for _, iss := range repo.Issues {
		if iss.State == "open" {
			open++
		}
	}
	var b []byte
	b = append(b, '{')
	b = appendJSONString(b, "id")
	b = append(b, ':')
	b = appendJSONInt(b, int64(repo.ID))
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, "R_"+strconv.Itoa(repo.ID))
	b = append(b, `,"name":`...)
	b = appendJSONString(b, repo.Name)
	b = append(b, `,"full_name":`...)
	b = appendJSONString(b, repo.Owner+"/"+repo.Name)
	b = append(b, `,"owner":`...)
	b = append(b, userJSON()...)
	b = append(b, `,"private":`...)
	b = appendJSONBool(b, repo.Private)
	b = append(b, `,"description":`...)
	b = appendJSONString(b, repo.Description)
	b = append(b, `,"fork":false,"created_at":`...)
	b = appendJSONString(b, created)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, created)
	b = append(b, `,"pushed_at":`...)
	b = appendJSONString(b, created)
	b = append(b, `,"homepage":"","size":0,"stargazers_count":0,"watchers_count":0,"language":null,"has_issues":true,"has_projects":true,"has_downloads":true,"has_wiki":true,"has_pages":false,"has_discussions":false,"forks_count":0,"open_issues_count":`...)
	b = appendJSONInt(b, int64(open))
	b = append(b, `,"license":null,"allow_forking":true,"default_branch":`...)
	b = appendJSONString(b, repo.DefaultBranch)
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/api/v3/repos/"+repo.Owner+"/"+repo.Name)
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/"+repo.Owner+"/"+repo.Name)
	b = append(b, '}')
	return b
}

func labelJSON(l *Label) []byte {
	var b []byte
	b = append(b, '{')
	b = appendJSONString(b, "name")
	b = append(b, ':')
	b = appendJSONString(b, l.Name)
	b = append(b, `,"color":`...)
	b = appendJSONString(b, l.Color)
	b = append(b, `,"description":`...)
	b = appendJSONString(b, l.Description)
	b = append(b, `,"default":false}`...)
	return b
}

func issueJSON(ctx RestCtx, repo *Repo, issue *Issue) []byte {
	var b []byte
	b = append(b, '{')
	b = appendJSONString(b, "id")
	b = append(b, ':')
	b = appendJSONInt(b, int64(issue.ID))
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, "I_"+strconv.Itoa(repo.ID)+"_"+strconv.Itoa(issue.Number))
	b = append(b, `,"number":`...)
	b = appendJSONInt(b, int64(issue.Number))
	b = append(b, `,"title":`...)
	b = appendJSONString(b, issue.Title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, issue.Body)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, issue.State)
	b = append(b, `,"labels":[`...)
	for i, name := range issue.Labels {
		if i > 0 {
			b = append(b, ',')
		}
		if l, ok := repo.Labels[name]; ok {
			b = append(b, labelJSON(l)...)
		} else {
			b = append(b, labelJSON(&Label{Name: name, Color: "ededed"})...)
		}
	}
	b = append(b, `],"user":`...)
	b = append(b, userJSON()...)
	b = append(b, `,"assignee":null,"assignees":[],"milestone":null,"locked":false,"comments":`...)
	b = appendJSONInt(b, int64(len(issue.Comments)))
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, isoStr(issue.CreatedSeq))
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, isoStr(issue.UpdatedSeq))
	b = append(b, `,"closed_at":`...)
	if issue.State == "closed" {
		b = appendJSONString(b, isoStr(issue.UpdatedSeq))
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"author_association":"OWNER","url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number))
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number))
	b = append(b, '}')
	return b
}

// issueBody returns the cached single-issue GET body (Node's issueBody).
func issueBody(ctx RestCtx, repo *Repo, issue *Issue) []byte {
	if issue.cachedBody != nil && issue.cachedHost == ctx.Host {
		return issue.cachedBody
	}
	body := issueJSON(ctx, repo, issue)
	issue.cachedBody = body
	issue.cachedHost = ctx.Host
	return body
}

func issueResult(ctx RestCtx, repo *Repo, issue *Issue, status int) Result {
	return jsonResult(status, issueBody(ctx, repo, issue), ctx.RequestID)
}

func pullJSON(ctx RestCtx, repo *Repo, pull *PullRequest) []byte {
	created := isoStr(pull.CreatedSeq)
	var b []byte
	b = append(b, '{')
	b = appendJSONString(b, "id")
	b = append(b, ':')
	b = appendJSONInt(b, int64(pull.ID))
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, "PR_"+strconv.Itoa(repo.ID)+"_"+strconv.Itoa(pull.Number))
	b = append(b, `,"number":`...)
	b = appendJSONInt(b, int64(pull.Number))
	b = append(b, `,"title":`...)
	b = appendJSONString(b, pull.Title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, pull.Body)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, pull.State)
	b = append(b, `,"merged":`...)
	b = appendJSONBool(b, pull.Merged)
	b = append(b, `,"merged_at":`...)
	if pull.MergedAt == "" {
		b = append(b, "null"...)
	} else {
		b = appendJSONString(b, pull.MergedAt)
	}
	b = append(b, `,"merge_commit_sha":`...)
	if pull.MergeCommitSha == "" {
		b = append(b, "null"...)
	} else {
		b = appendJSONString(b, pull.MergeCommitSha)
	}
	b = append(b, `,"head":{"ref":`...)
	b = appendJSONString(b, pull.Head)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, pull.HeadSha)
	b = append(b, `,"label":`...)
	b = appendJSONString(b, repo.Owner+":"+pull.Head)
	b = append(b, `},"base":{"ref":`...)
	b = appendJSONString(b, pull.Base)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, pull.BaseSha)
	b = append(b, `,"label":`...)
	b = appendJSONString(b, repo.Owner+":"+pull.Base)
	b = append(b, `},"user":`...)
	b = append(b, userJSON()...)
	b = append(b, `,"labels":[],"assignee":null,"assignees":[],"milestone":null,"locked":false,"draft":false,"comments":0,"review_comments":0,"commits":1,"additions":0,"deletions":0,"changed_files":0,"created_at":`...)
	b = appendJSONString(b, created)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, isoStr(pull.UpdatedSeq))
	b = append(b, `,"closed_at":`...)
	if pull.State == "closed" {
		b = appendJSONString(b, isoStr(pull.UpdatedSeq))
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls/"+strconv.Itoa(pull.Number))
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/"+repo.Owner+"/"+repo.Name+"/pull/"+strconv.Itoa(pull.Number))
	b = append(b, '}')
	return b
}

func commitJSON(ctx RestCtx, repo *Repo, oid string) []byte {
	obj := repo.Objects[oid]
	var commit *CommitView
	if obj != nil && obj.Type == objCommit {
		c := parseCommit(obj.Data)
		commit = &c
	}
	timestamp := isoStr(epochSec + 1)
	if commit != nil {
		// author line ends with " <epoch> +0000"
		if idx := strings.LastIndex(commit.Author, " "); idx >= 0 {
			rest := commit.Author[idx+1:]
			if strings.HasSuffix(commit.Author, " +0000") {
				// rest is "+0000"; the number is the token before it
				trimmed := strings.TrimSuffix(commit.Author, " +0000")
				if j := strings.LastIndex(trimmed, " "); j >= 0 {
					if v, err := strconv.ParseInt(trimmed[j+1:], 10, 64); err == nil {
						timestamp = isoStr(v)
					}
				}
			}
			_ = rest
		}
	}
	var message string
	var parents []string
	tree := zeroOID
	if commit != nil {
		message = commit.Message
		tree = commit.Tree
		for _, p := range commit.Parents {
			parents = append(parents, p)
		}
	}
	var b []byte
	b = append(b, '{')
	b = appendJSONString(b, "sha")
	b = append(b, ':')
	b = appendJSONString(b, oid)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, "C_"+oid)
	b = append(b, `,"commit":{"message":`...)
	b = appendJSONString(b, message)
	b = append(b, `,"author":{"name":`...)
	b = appendJSONString(b, viewerName)
	b = append(b, `,"email":`...)
	b = appendJSONString(b, viewerEmail)
	b = append(b, `,"date":`...)
	b = appendJSONString(b, timestamp)
	b = append(b, `},"committer":{"name":`...)
	b = appendJSONString(b, viewerName)
	b = append(b, `,"email":`...)
	b = appendJSONString(b, viewerEmail)
	b = append(b, `,"date":`...)
	b = appendJSONString(b, timestamp)
	b = append(b, `},"tree":{"sha":`...)
	b = appendJSONString(b, tree)
	b = append(b, `},"comment_count":0},"author":`...)
	b = append(b, userJSON()...)
	b = append(b, `,"committer":`...)
	b = append(b, userJSON()...)
	b = append(b, `,"parents":[`...)
	for i, p := range parents {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"sha":`...)
		b = appendJSONString(b, p)
		b = append(b, '}')
	}
	b = append(b, `],"url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits/"+oid)
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "http://"+ctx.Host+"/"+repo.Owner+"/"+repo.Name+"/commit/"+oid)
	b = append(b, '}')
	return b
}

func resolveRef(repo *Repo, ref string) string {
	if isOID(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref
		}
		return ""
	}
	if strings.HasPrefix(ref, "refs/") {
		if oid, ok := repo.Refs[ref]; ok {
			return oid
		}
		return ""
	}
	if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		return oid
	}
	if oid, ok := repo.Refs["refs/tags/"+ref]; ok {
		return oid
	}
	return ""
}

func commitListFrom(repo *Repo, start string, limit int) []string {
	var out []string
	seen := make(map[string]struct{})
	queue := []string{start}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		if _, ok := seen[oid]; ok {
			continue
		}
		seen[oid] = struct{}{}
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		out = append(out, oid)
		c := parseCommit(obj.Data)
		queue = append(queue, c.Parents...)
	}
	return out
}

func filesAt(repo *Repo, commitOID string) map[string]FlatFile {
	obj := repo.Objects[commitOID]
	if obj == nil || obj.Type != objCommit {
		return make(map[string]FlatFile)
	}
	return flattenTree(repo, parseCommit(obj.Data).Tree)
}

func mergedFiles(baseTip, baseFiles, headFiles map[string]FlatFile) map[string]FlatFile {
	out := make(map[string]FlatFile, len(baseTip))
	for k, v := range baseTip {
		out[k] = v
	}
	for _, ch := range diffFlat(baseFiles, headFiles) {
		if ch.Status == "removed" {
			delete(out, ch.Path)
		} else if ch.NewOID != "" {
			mode := "100644"
			if f, ok := headFiles[ch.Path]; ok {
				mode = f.Mode
			}
			out[ch.Path] = FlatFile{Mode: mode, OID: ch.NewOID}
		}
	}
	return out
}

func prConflicts(repo *Repo, pull *PullRequest) bool {
	headTip := pull.HeadSha
	baseTip := pull.BaseSha
	if v, ok := repo.Refs["refs/heads/"+pull.Base]; ok {
		baseTip = v
	}
	if headTip == zeroOID || baseTip == zeroOID {
		return false
	}
	base := mergeBase(repo, headTip, baseTip)
	if base == "" {
		return false
	}
	baseFiles := filesAt(repo, base)
	headFiles := filesAt(repo, headTip)
	baseTipFiles := filesAt(repo, baseTip)
	headChanged := make(map[string]string)
	for _, ch := range diffFlat(baseFiles, headFiles) {
		headChanged[ch.Path] = ch.NewOID
	}
	for _, ch := range diffFlat(baseFiles, baseTipFiles) {
		if hv, ok := headChanged[ch.Path]; ok && hv != ch.NewOID {
			return true
		}
	}
	return false
}

func lineCounts(oldText, newText *string) (additions, deletions int) {
	countLines := func(text *string) map[string]int {
		m := make(map[string]int)
		if text == nil {
			return m
		}
		for _, line := range strings.Split(*text, "\n") {
			m[line]++
		}
		return m
	}
	oldLines := countLines(oldText)
	newLines := countLines(newText)
	for line, count := range newLines {
		if oc := oldLines[line]; count > oc {
			additions += count - oc
		}
	}
	for line, count := range oldLines {
		if nc := newLines[line]; count > nc {
			deletions += count - nc
		}
	}
	return additions, deletions
}

func blobText(repo *Repo, oid string) *string {
	if oid == "" {
		return nil
	}
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != objBlob {
		return nil
	}
	s := string(obj.Data)
	return &s
}

// body helpers: parsed JSON object body (map) or nil.
func bodyStr(body map[string]interface{}, key string) (string, bool) {
	if body == nil {
		return "", false
	}
	v, ok := body[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func bodyBool(body map[string]interface{}, key string) bool {
	if body == nil {
		return false
	}
	v, ok := body[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// handleRestLocked is the REST dispatcher; caller holds the store lock.
func handleRestLocked(ctx RestCtx, method string, segments []string, query url.Values, body map[string]interface{}) Result {
	store := ctx.Store
	rid := ctx.RequestID
	notFound := func() Result { return errorResult(404, "Not Found", rid) }

	// GET /api/v3/
	if method == "GET" && len(segments) == 0 {
		b := []byte(`{"current_user_url":"http://` + ctx.Host + `/api/v3/user","repository_url":"http://` + ctx.Host + `/api/v3/repos/{owner}/{repo}","user_url":"http://` + ctx.Host + `/api/v3/users/{user}"}`)
		return jsonResult(200, b, rid, [2]string{"X-OAuth-Scopes", "repo, user"})
	}
	// GET /api/v3/meta
	if method == "GET" && len(segments) == 1 && segments[0] == "meta" {
		return jsonResult(200, []byte(`{"installed_version":"1.0.0","verifiable_password_authentication":true}`), rid)
	}
	// GET /api/v3/user
	if method == "GET" && len(segments) == 1 && segments[0] == "user" {
		return jsonResult(200, userJSON(), rid)
	}
	// /user/repos
	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if method == "POST" {
			name, _ := bodyStr(body, "name")
			if name == "" {
				return errorResult(422, "name is required", rid)
			}
			if store.getRepoLocked(viewerLogin, name) != nil {
				return errorResult(422, "Repository "+viewerLogin+"/"+name+" already exists", rid)
			}
			desc, _ := bodyStr(body, "description")
			repo := store.createRepoLocked(viewerLogin, name, desc, bodyBool(body, "private"))
			return jsonResult(201, repoJSON(ctx, repo), rid)
		}
		if method == "GET" {
			page, perPage := pageParams(query)
			repos := store.listViewerReposLocked()
			start := (page - 1) * perPage
			end := start + perPage
			if end > len(repos) {
				end = len(repos)
			}
			var slice []*Repo
			if start < len(repos) {
				slice = repos[start:end]
			}
			hasNext := start+perPage < len(repos)
			hasPrev := page > 1
			link := linkHeader(ctx.Host, "/api/v3/user/repos", nil, page, perPage, hasNext, hasPrev)
			var b []byte
			b = append(b, '[')
			for i, r := range slice {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, repoJSON(ctx, r)...)
			}
			b = append(b, ']')
			var extra [][2]string
			if link != "" {
				extra = append(extra, [2]string{"Link", link})
			}
			return jsonResult(200, b, rid, extra...)
		}
		return notFound()
	}

	if len(segments) < 3 || segments[0] != "repos" {
		return notFound()
	}
	owner := segments[1]
	repoName := segments[2]
	repo := store.getRepoLocked(owner, repoName)
	if repo == nil {
		return notFound()
	}
	rest := segments[3:]

	// GET /repos/{o}/{r}
	if method == "GET" && len(rest) == 0 {
		return jsonResult(200, repoJSON(ctx, repo), rid)
	}
	// PATCH /repos/{o}/{r}
	if method == "PATCH" && len(rest) == 0 {
		if desc, ok := bodyStr(body, "description"); ok {
			repo.Description = desc
		}
		if name, ok := bodyStr(body, "name"); ok && name != repo.Name {
			delete(store.repos, store.key(owner, repoName))
			repo.Name = name
			store.repos[store.key(owner, repo.Name)] = repo
			clearAllIssueCaches(repo)
		}
		return jsonResult(200, repoJSON(ctx, repo), rid)
	}
	// DELETE /repos/{o}/{r}
	if method == "DELETE" && len(rest) == 0 {
		delete(store.repos, store.key(owner, repoName))
		if r := store.lastRepo.Load(); r != nil && r.Owner == owner && r.Name == repoName {
			store.lastRepo.Store(nil)
		}
		return Result{Status: 204, Body: []byte{}, Extra: [][2]string{{"X-GitHub-Request-Id", rid}}}
	}

	// Branches.
	if len(rest) > 0 && rest[0] == "branches" {
		if method == "GET" && len(rest) == 1 {
			type br struct {
				name string
				oid  string
			}
			var branches []br
			for name, oid := range repo.Refs {
				if strings.HasPrefix(name, "refs/heads/") {
					branches = append(branches, br{strings.TrimPrefix(name, "refs/heads/"), oid})
				}
			}
			sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
			var b []byte
			b = append(b, '[')
			for i, x := range branches {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"name":`...)
				b = appendJSONString(b, x.name)
				b = append(b, `,"commit":{"sha":`...)
				b = appendJSONString(b, x.oid)
				b = append(b, `},"protected":false}`...)
			}
			b = append(b, ']')
			return jsonResult(200, b, rid)
		}
		if method == "GET" && len(rest) == 2 {
			oid, ok := repo.Refs["refs/heads/"+rest[1]]
			if !ok {
				return notFound()
			}
			b := []byte(`{"name":`)
			b = appendJSONString(b, rest[1])
			b = append(b, `,"commit":{"sha":`...)
			b = appendJSONString(b, oid)
			b = append(b, `},"protected":false}`...)
			return jsonResult(200, b, rid)
		}
		return notFound()
	}

	// Commits.
	if len(rest) > 0 && rest[0] == "commits" && method == "GET" {
		page, perPage := pageParams(query)
		ref := query.Get("sha")
		var start string
		if ref == "" {
			tip, ok := headTarget(repo)
			if !ok {
				return notFound()
			}
			start = tip
		} else {
			start = resolveRef(repo, ref)
			if start == "" {
				return notFound()
			}
		}
		list := commitListFrom(repo, start, 1000)
		s := (page - 1) * perPage
		e := s + perPage
		if e > len(list) {
			e = len(list)
		}
		var slice []string
		if s < len(list) {
			slice = list[s:e]
		}
		hasNext := s+perPage < len(list)
		hasPrev := page > 1
		link := linkHeader(ctx.Host, "/api/v3/repos/"+owner+"/"+repoName+"/commits", [][2]string{{"sha", ref}}, page, perPage, hasNext, hasPrev)
		var b []byte
		b = append(b, '[')
		for i, oid := range slice {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, commitJSON(ctx, repo, oid)...)
		}
		b = append(b, ']')
		var extra [][2]string
		if link != "" {
			extra = append(extra, [2]string{"Link", link})
		}
		return jsonResult(200, b, rid, extra...)
	}

	// git refs.
	if len(rest) > 1 && rest[0] == "git" && rest[1] == "refs" {
		if method == "POST" && len(rest) == 2 {
			ref, _ := bodyStr(body, "ref")
			sha, _ := bodyStr(body, "sha")
			if !strings.HasPrefix(ref, "refs/") || !isOID(sha) {
				return errorResult(422, "ref or sha is invalid", rid)
			}
			if _, ok := repo.Objects[sha]; !ok {
				return errorResult(422, "ref or sha is invalid", rid)
			}
			repo.Refs[ref] = sha
			b := []byte(`{"ref":`)
			b = appendJSONString(b, ref)
			b = append(b, `,"object":{"sha":`...)
			b = appendJSONString(b, sha)
			b = append(b, `},"node_id":`...)
			b = appendJSONString(b, "REF_"+strconv.Itoa(repo.ID)+"_"+ref)
			b = append(b, '}')
			return jsonResult(201, b, rid)
		}
		if method == "GET" && len(rest) >= 3 {
			ref := strings.Join(rest[2:], "/")
			oid, ok := repo.Refs[ref]
			if !ok {
				return notFound()
			}
			b := []byte(`{"ref":`)
			b = appendJSONString(b, ref)
			b = append(b, `,"object":{"sha":`...)
			b = appendJSONString(b, oid)
			b = append(b, "}}"...)
			return jsonResult(200, b, rid)
		}
		return notFound()
	}

	// Contents.
	if len(rest) > 0 && rest[0] == "contents" {
		return handleContents(ctx, store, repo, owner, repoName, rest, method, query, body)
	}

	// Labels.
	if len(rest) > 0 && rest[0] == "labels" {
		if method == "GET" && len(rest) == 1 {
			names := make([]string, 0, len(repo.Labels))
			for n := range repo.Labels {
				names = append(names, n)
			}
			sort.Strings(names)
			var b []byte
			b = append(b, '[')
			for i, n := range names {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, labelJSON(repo.Labels[n])...)
			}
			b = append(b, ']')
			return jsonResult(200, b, rid)
		}
		if method == "POST" && len(rest) == 1 {
			name, _ := bodyStr(body, "name")
			if name == "" {
				return errorResult(422, "name is required", rid)
			}
			color, _ := bodyStr(body, "color")
			if color == "" {
				color = "ededed"
			}
			desc, _ := bodyStr(body, "description")
			label := &Label{Name: name, Color: color, Description: desc}
			repo.Labels[name] = label
			clearAllIssueCaches(repo)
			return jsonResult(201, labelJSON(label), rid)
		}
		return notFound()
	}

	// Issues.
	if len(rest) > 0 && rest[0] == "issues" {
		return handleIssues(ctx, store, repo, owner, repoName, rest, method, query, body)
	}

	// Pull requests.
	if len(rest) > 0 && rest[0] == "pulls" {
		return handlePulls(ctx, store, repo, owner, repoName, rest, method, query, body)
	}

	return notFound()
}

func handleContents(ctx RestCtx, store *Store, repo *Repo, owner, repoName string, rest []string, method string, query url.Values, body map[string]interface{}) Result {
	rid := ctx.RequestID
	notFound := func() Result { return errorResult(404, "Not Found", rid) }
	path := strings.Join(rest[1:], "/")
	if path == "" {
		return notFound()
	}
	refParam := query.Get("ref")
	var baseCommit string
	if refParam == "" {
		if tip, ok := headTarget(repo); ok {
			baseCommit = tip
		}
	} else {
		baseCommit = resolveRef(repo, refParam)
	}
	files := make(map[string]FlatFile)
	if baseCommit != "" {
		files = filesAt(repo, baseCommit)
	}

	if method == "GET" {
		if entry, ok := files[path]; ok {
			obj := repo.Objects[entry.OID]
			if obj == nil {
				return notFound()
			}
			var b []byte
			b = append(b, `{"name":`...)
			base := path
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				base = path[idx+1:]
			}
			b = appendJSONString(b, base)
			b = append(b, `,"path":`...)
			b = appendJSONString(b, path)
			b = append(b, `,"sha":`...)
			b = appendJSONString(b, entry.OID)
			b = append(b, `,"size":`...)
			b = appendJSONInt(b, int64(len(obj.Data)))
			b = append(b, `,"type":"file","encoding":"base64","content":`...)
			b = appendJSONString(b, base64.StdEncoding.EncodeToString(obj.Data))
			b = append(b, `,"url":`...)
			b = appendJSONString(b, "http://"+ctx.Host+"/api/v3/repos/"+owner+"/"+repoName+"/contents/"+path)
			b = append(b, `,"html_url":`...)
			b = appendJSONString(b, "http://"+ctx.Host+"/"+owner+"/"+repoName+"/blob/"+repo.DefaultBranch+"/"+path)
			b = append(b, '}')
			return jsonResult(200, b, rid)
		}
		type child struct {
			typ  string
			name string
			path string
			sha  string
		}
		children := make(map[string]child)
		for filePath, file := range files {
			if !strings.HasPrefix(filePath, path+"/") {
				continue
			}
			remainder := filePath[len(path)+1:]
			first := strings.SplitN(remainder, "/", 2)[0]
			if strings.Contains(remainder, "/") {
				children[first] = child{"dir", first, path + "/" + first, ""}
			} else {
				children[first] = child{"file", first, filePath, file.OID}
			}
		}
		if len(children) == 0 {
			return notFound()
		}
		items := make([]child, 0, len(children))
		for _, c := range children {
			items = append(items, c)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
		var b []byte
		b = append(b, '[')
		for i, item := range items {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"type":`...)
			b = appendJSONString(b, item.typ)
			b = append(b, `,"name":`...)
			b = appendJSONString(b, item.name)
			b = append(b, `,"path":`...)
			b = appendJSONString(b, item.path)
			b = append(b, `,"sha":`...)
			b = appendJSONString(b, item.sha)
			b = append(b, `,"size":0,"url":`...)
			b = appendJSONString(b, "http://"+ctx.Host+"/api/v3/repos/"+owner+"/"+repoName+"/contents/"+item.path)
			b = append(b, '}')
		}
		b = append(b, ']')
		return jsonResult(200, b, rid)
	}

	if method == "PUT" || method == "DELETE" {
		message, _ := bodyStr(body, "message")
		if message == "" {
			if method == "PUT" {
				message = "Update " + path
			} else {
				message = "Delete " + path
			}
		}
		branch, _ := bodyStr(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		branchRef := "refs/heads/" + branch
		branchTip, hasBranch := repo.Refs[branchRef]
		if !hasBranch {
			if tip, ok := headTarget(repo); ok {
				branchTip = tip
				hasBranch = true
			}
		}
		currentFiles := make(map[string]FlatFile)
		if hasBranch {
			currentFiles = filesAt(repo, branchTip)
		}
		if method == "PUT" {
			contentB64, _ := bodyStr(body, "content")
			decoded, _ := base64.StdEncoding.DecodeString(contentB64)
			exists := false
			var currentSha string
			if f, ok := currentFiles[path]; ok {
				exists = true
				currentSha = f.OID
			}
			sha, hasSha := bodyStr(body, "sha")
			if exists {
				if !hasSha || sha != currentSha {
					return errorResult(409, "sha does not match the current blob", rid)
				}
			}
			blobOID := putObject(repo, objBlob, decoded)
			nextFiles := make(map[string]FlatFile, len(currentFiles)+1)
			for k, v := range currentFiles {
				nextFiles[k] = v
			}
			nextFiles[path] = FlatFile{Mode: "100644", OID: blobOID}
			tree := buildTreeFromFlat(repo, nextFiles)
			var parents []string
			if hasBranch {
				parents = []string{branchTip}
			}
			commit := makeServerCommit(store, repo, tree, parents, message)
			repo.Refs[branchRef] = commit
			status := 201
			if exists {
				status = 200
			}
			var b []byte
			b = append(b, `{"content":{"name":`...)
			base := path
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				base = path[idx+1:]
			}
			b = appendJSONString(b, base)
			b = append(b, `,"path":`...)
			b = appendJSONString(b, path)
			b = append(b, `,"sha":`...)
			b = appendJSONString(b, blobOID)
			b = append(b, `,"size":`...)
			b = appendJSONInt(b, int64(len(decoded)))
			b = append(b, `,"type":"file","encoding":"base64","content":`...)
			b = appendJSONString(b, contentB64)
			b = append(b, `},"commit":{"sha":`...)
			b = appendJSONString(b, commit)
			b = append(b, `,"message":`...)
			b = appendJSONString(b, message)
			b = append(b, "}}"...)
			return jsonResult(status, b, rid)
		}
		// DELETE
		if _, ok := currentFiles[path]; !ok {
			return notFound()
		}
		currentSha := currentFiles[path].OID
		if sha, hasSha := bodyStr(body, "sha"); hasSha && sha != currentSha {
			return errorResult(409, "sha does not match the current blob", rid)
		}
		nextFiles := make(map[string]FlatFile, len(currentFiles))
		for k, v := range currentFiles {
			nextFiles[k] = v
		}
		delete(nextFiles, path)
		tree := buildTreeFromFlat(repo, nextFiles)
		var parents []string
		if hasBranch {
			parents = []string{branchTip}
		}
		commit := makeServerCommit(store, repo, tree, parents, message)
		repo.Refs[branchRef] = commit
		var b []byte
		b = append(b, `{"commit":{"sha":`...)
		b = appendJSONString(b, commit)
		b = append(b, `,"message":`...)
		b = appendJSONString(b, message)
		b = append(b, "}}"...)
		return jsonResult(200, b, rid)
	}
	return notFound()
}

func handleIssues(ctx RestCtx, store *Store, repo *Repo, owner, repoName string, rest []string, method string, query url.Values, body map[string]interface{}) Result {
	rid := ctx.RequestID
	notFound := func() Result { return errorResult(404, "Not Found", rid) }
	if len(rest) == 1 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			sortBy := query.Get("sort")
			if sortBy == "" {
				sortBy = "created"
			}
			direction := query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			var list []*Issue
			for _, iss := range repo.IssueOrder {
				if state != "all" && iss.State != state {
					continue
				}
				list = append(list, iss)
			}
			sort.SliceStable(list, func(i, j int) bool {
				var ka, kb int64
				if sortBy == "created" {
					ka, kb = list[i].CreatedSeq, list[j].CreatedSeq
				} else {
					ka, kb = list[i].UpdatedSeq, list[j].UpdatedSeq
				}
				if direction == "asc" {
					return ka < kb
				}
				return ka > kb
			})
			page, perPage := pageParams(query)
			s := (page - 1) * perPage
			e := s + perPage
			if e > len(list) {
				e = len(list)
			}
			var slice []*Issue
			if s < len(list) {
				slice = list[s:e]
			}
			hasNext := s+perPage < len(list)
			hasPrev := page > 1
			link := linkHeader(ctx.Host, "/api/v3/repos/"+owner+"/"+repoName+"/issues",
				[][2]string{{"state", state}, {"sort", sortBy}, {"direction", direction}}, page, perPage, hasNext, hasPrev)
			var b []byte
			b = append(b, '[')
			for i, iss := range slice {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, issueJSON(ctx, repo, iss)...)
			}
			b = append(b, ']')
			var extra [][2]string
			if link != "" {
				extra = append(extra, [2]string{"Link", link})
			}
			return jsonResult(200, b, rid, extra...)
		}
		if method == "POST" {
			title, _ := bodyStr(body, "title")
			if title == "" {
				return errorResult(422, "title is required", rid)
			}
			var labels []string
			if raw, ok := body["labels"]; ok {
				if arr, ok := raw.([]interface{}); ok {
					for _, item := range arr {
						var name string
						switch v := item.(type) {
						case string:
							name = v
						case map[string]interface{}:
							if n, ok := v["name"].(string); ok {
								name = n
							}
						}
						if name == "" {
							continue
						}
						if _, ok := repo.Labels[name]; !ok {
							return errorResult(422, "label does not exist: "+name, rid)
						}
						labels = append(labels, name)
					}
				}
			}
			number := repo.NextNumber
			repo.NextNumber++
			seq := store.tickLocked()
			issueBodyStr, _ := bodyStr(body, "body")
			issue := &Issue{
				ID: number, Number: number, Title: title, Body: issueBodyStr,
				State: "open", Labels: labels, User: viewerLogin,
				CreatedSeq: seq, UpdatedSeq: seq, Comments: nil,
			}
			repo.Issues[number] = issue
			repo.IssueOrder = append(repo.IssueOrder, issue)
			return issueResult(ctx, repo, issue, 201)
		}
		return notFound()
	}
	number, ok := parseJSInt(rest[1])
	if !ok {
		return notFound()
	}
	issue, ok := repo.Issues[number]
	if !ok {
		return notFound()
	}
	if len(rest) == 2 {
		if method == "GET" {
			return issueResult(ctx, repo, issue, 200)
		}
		if method == "PATCH" {
			if st, ok := bodyStr(body, "state"); ok && (st == "open" || st == "closed") {
				issue.State = st
				issue.UpdatedSeq = store.tickLocked()
			}
			if t, ok := bodyStr(body, "title"); ok {
				issue.Title = t
			}
			if bdy, ok := bodyStr(body, "body"); ok {
				issue.Body = bdy
			}
			clearIssueCache(issue)
			return issueResult(ctx, repo, issue, 200)
		}
		return notFound()
	}
	if len(rest) == 3 && rest[2] == "comments" {
		if method == "GET" {
			var b []byte
			b = append(b, '[')
			for i, c := range issue.Comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"id":`...)
				b = appendJSONInt(b, int64(c.ID))
				b = append(b, `,"body":`...)
				b = appendJSONString(b, c.Body)
				b = append(b, `,"user":`...)
				b = append(b, userJSON()...)
				b = append(b, `,"created_at":`...)
				b = appendJSONString(b, isoStr(c.CreatedSeq))
				b = append(b, `,"updated_at":`...)
				b = appendJSONString(b, isoStr(c.CreatedSeq))
				b = append(b, '}')
			}
			b = append(b, ']')
			return jsonResult(200, b, rid)
		}
		if method == "POST" {
			commentBody, _ := bodyStr(body, "body")
			comment := &IssueComment{ID: repo.NextCommentID, Body: commentBody, User: viewerLogin, CreatedSeq: store.tickLocked()}
			repo.NextCommentID++
			issue.Comments = append(issue.Comments, comment)
			clearIssueCache(issue)
			var b []byte
			b = append(b, `{"id":`...)
			b = appendJSONInt(b, int64(comment.ID))
			b = append(b, `,"body":`...)
			b = appendJSONString(b, comment.Body)
			b = append(b, `,"user":`...)
			b = append(b, userJSON()...)
			b = append(b, `,"created_at":`...)
			b = appendJSONString(b, isoStr(comment.CreatedSeq))
			b = append(b, '}')
			return jsonResult(201, b, rid)
		}
		return notFound()
	}
	return notFound()
}

func handlePulls(ctx RestCtx, store *Store, repo *Repo, owner, repoName string, rest []string, method string, query url.Values, body map[string]interface{}) Result {
	rid := ctx.RequestID
	notFound := func() Result { return errorResult(404, "Not Found", rid) }
	if len(rest) == 1 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			var list []*PullRequest
			for _, p := range repo.PullOrder {
				if state != "all" && p.State != state {
					continue
				}
				list = append(list, p)
			}
			sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
			page, perPage := pageParams(query)
			s := (page - 1) * perPage
			e := s + perPage
			if e > len(list) {
				e = len(list)
			}
			var slice []*PullRequest
			if s < len(list) {
				slice = list[s:e]
			}
			hasNext := s+perPage < len(list)
			hasPrev := page > 1
			link := linkHeader(ctx.Host, "/api/v3/repos/"+owner+"/"+repoName+"/pulls",
				[][2]string{{"state", state}}, page, perPage, hasNext, hasPrev)
			var b []byte
			b = append(b, '[')
			for i, p := range slice {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, pullJSON(ctx, repo, p)...)
			}
			b = append(b, ']')
			var extra [][2]string
			if link != "" {
				extra = append(extra, [2]string{"Link", link})
			}
			return jsonResult(200, b, rid, extra...)
		}
		if method == "POST" {
			title, _ := bodyStr(body, "title")
			head, _ := bodyStr(body, "head")
			base, _ := bodyStr(body, "base")
			if title == "" || head == "" || base == "" {
				return errorResult(422, "title, head and base are required", rid)
			}
			headSha, okH := repo.Refs["refs/heads/"+head]
			baseSha, okB := repo.Refs["refs/heads/"+base]
			if !okH || !okB {
				return errorResult(422, "head or base branch does not exist", rid)
			}
			for _, p := range repo.PullOrder {
				if p.State == "open" && p.Head == head && p.Base == base {
					return errorResult(422, "a pull request already exists for this head", rid)
				}
			}
			number := repo.NextNumber
			repo.NextNumber++
			seq := store.tickLocked()
			bodyStr, _ := bodyStr(body, "body")
			pull := &PullRequest{
				ID: number, Number: number, Title: title, Body: bodyStr,
				State: "open", Head: head, Base: base,
				HeadSha: headSha, BaseSha: baseSha,
				User: viewerLogin, CreatedSeq: seq, UpdatedSeq: seq,
			}
			repo.Pulls[number] = pull
			repo.PullOrder = append(repo.PullOrder, pull)
			return jsonResult(201, pullJSON(ctx, repo, pull), rid)
		}
		return notFound()
	}
	number, ok := parseJSInt(rest[1])
	if !ok {
		return notFound()
	}
	pull, ok := repo.Pulls[number]
	if !ok {
		return notFound()
	}
	if len(rest) == 2 {
		if method == "GET" {
			return jsonResult(200, pullJSON(ctx, repo, pull), rid)
		}
		return notFound()
	}
	if len(rest) == 3 && rest[2] == "files" && method == "GET" {
		base := mergeBase(repo, pull.HeadSha, pull.BaseSha)
		baseFiles := make(map[string]FlatFile)
		if base != "" {
			baseFiles = filesAt(repo, base)
		}
		headFiles := filesAt(repo, pull.HeadSha)
		changes := diffFlat(baseFiles, headFiles)
		var b []byte
		b = append(b, '[')
		for i, ch := range changes {
			if i > 0 {
				b = append(b, ',')
			}
			add, del := lineCounts(blobText(repo, ch.OldOID), blobText(repo, ch.NewOID))
			sha := ch.NewOID
			if sha == "" {
				sha = ch.OldOID
			}
			b = append(b, `{"sha":`...)
			b = appendJSONString(b, sha)
			b = append(b, `,"filename":`...)
			b = appendJSONString(b, ch.Path)
			b = append(b, `,"status":`...)
			b = appendJSONString(b, ch.Status)
			b = append(b, `,"additions":`...)
			b = appendJSONInt(b, int64(add))
			b = append(b, `,"deletions":`...)
			b = appendJSONInt(b, int64(del))
			b = append(b, `,"changes":`...)
			b = appendJSONInt(b, int64(add+del))
			b = append(b, `,"blob_url":"","raw_url":"","contents_url":""}`...)
		}
		b = append(b, ']')
		return jsonResult(200, b, rid)
	}
	if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
		if pull.State != "open" {
			return errorResult(405, "pull request is not open", rid)
		}
		if prConflicts(repo, pull) {
			return errorResult(405, "merge conflict", rid)
		}
		mergeMethod, _ := bodyStr(body, "merge_method")
		if mergeMethod == "" {
			mergeMethod = "merge"
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
		if mergeMethod == "squash" {
			parents = []string{baseTipSha}
			message = strings.TrimSpace(pull.Title + " (#" + strconv.Itoa(pull.Number) + ")\n\n" + pull.Body)
		} else {
			parents = []string{baseTipSha, pull.HeadSha}
			message = "Merge pull request #" + strconv.Itoa(pull.Number) + " from " + repo.Owner + "/" + pull.Head + "\n\n" + pull.Title
		}
		commit := makeServerCommit(store, repo, tree, parents, message)
		repo.Refs["refs/heads/"+pull.Base] = commit
		pull.State = "closed"
		pull.Merged = true
		pull.MergedAt = isoStr(epochSec + store.seq)
		pull.MergeCommitSha = commit
		pull.UpdatedSeq = store.tickLocked()
		var b []byte
		b = append(b, `{"sha":`...)
		b = appendJSONString(b, commit)
		b = append(b, `,"merged":true,"message":`...)
		b = appendJSONString(b, message)
		b = append(b, '}')
		return jsonResult(200, b, rid)
	}
	return notFound()
}
