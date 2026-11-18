// In-memory store: one shared object store per repo, plus REST resources.
// Everything is deterministic: ids and timestamps come from monotonic
// counters over a fixed base time (MAKEFUN_TRIAL_NOW when set, else the
// scenario epoch). Ported from the Node store.ts; behavior is identical.

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const ScenarioEpochSec = 1735689600

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func hashObject(typ string, data []byte) string {
	head := []byte(typ + " " + strconv.Itoa(len(data)) + "\x00")
	h := sha1.New()
	h.Write(head)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	Data []byte
}

type Comment struct {
	ID  int
	Body string
	Seq int
}

type Issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Seq      int
	Comments []*Comment
	// Cached template-serialized JSON (rest.go issueText); cleared by every
	// mutation of the issue. Derived bytes only.
	JSONCache     string
	JSONCacheSet  bool
}

type PullFile struct {
	Filename  string
	Status    string // added | modified | removed
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	ID            int
	Number        int
	Title         string
	Body          string
	State         string // open | closed
	Merged        bool
	MergedAt      string // "" when null
	MergeCommitSha string // "" when null
	HeadRef       string
	BaseRef       string
	HeadSha       string
	BaseSha       string
	MergeBaseSha  string // "" when null
	Files         []PullFile
	Seq           int
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int
	Objects       map[string]*GitObject
	Refs          map[string]string
	Issues        map[int]*Issue
	IssueSeq      int
	Pulls         map[int]*Pull
	PullSeq       int
	Labels        map[string]Label
	CommentSeq    int
}

type Label struct {
	Name  string
	Color string
}

type Store struct {
	ViewerLogin string
	ViewerName  string
	ViewerEmail string
	ViewerID    int
	BaseEpochSec int64
	Seq         int
	RepoSeq     int
	IssueIDSeq  int
	CommentIDSeq int
	PullIDSeq   int
	Repos       map[string]*Repo
	ReqSeq      int
	// Bumped once per completed mutating request; response caches key on it.
	Version int
}

func (s *Store) BumpVersion() { s.Version++ }

func NewStore(viewer ScenarioUser) *Store {
	epoch := int64(ScenarioEpochSec)
	if now := os.Getenv("MAKEFUN_TRIAL_NOW"); now != "" {
		if t, err := time.Parse(time.RFC3339, now); err == nil {
			epoch = t.Unix()
		} else if n, err := strconv.ParseFloat(now, 64); err == nil && n > 0 {
			if n > 1e12 {
				epoch = int64(n / 1000)
			} else {
				epoch = int64(n)
			}
		}
	}
	return &Store{
		ViewerLogin: viewer.Login,
		ViewerName:  viewer.Name,
		ViewerEmail: viewer.Email,
		ViewerID:    viewer.ID,
		BaseEpochSec: epoch,
		Repos:       make(map[string]*Repo),
	}
}

func (s *Store) NextSeq() int {
	s.Seq++
	return s.Seq
}

// TsIso is the ISO timestamp for creation sequence n (deterministic).
func (s *Store) TsIso(seq int) string {
	return time.Unix(s.BaseEpochSec+int64(seq), 0).UTC().Format("2006-01-02T15:04:05Z")
}

// TsEpoch is epoch seconds for creation sequence n (deterministic).
func (s *Store) TsEpoch(seq int) int64 {
	return s.BaseEpochSec + int64(seq)
}

func (s *Store) RepoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[s.RepoKey(owner, name)]
}

func (s *Store) CreateRepo(owner, name, description string, isPrivate bool) *Repo {
	s.RepoSeq++
	repo := &Repo{
		ID:            5000 + s.RepoSeq - 1,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		CreatedSeq:    s.NextSeq(),
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		Labels:        make(map[string]Label),
	}
	s.Repos[s.RepoKey(owner, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	_, ok := s.Repos[s.RepoKey(owner, name)]
	if ok {
		delete(s.Repos, s.RepoKey(owner, name))
	}
	return ok
}

func (s *Store) ListViewerRepos() []*Repo {
	out := []*Repo{}
	for _, repo := range s.Repos {
		if repo.Owner == s.ViewerLogin {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// ---- git objects -------------------------------------------------------

func (s *Store) PutObject(repo *Repo, typ ObjType, data []byte) string {
	oid := hashObject(string(typ), data)
	if _, ok := repo.Objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		repo.Objects[oid] = &GitObject{Type: typ, Data: cp}
	}
	return oid
}

func (s *Store) GetObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree object into entries.
func (s *Store) ParseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	i := 0
	for i < len(data) {
		sp := indexByte(data, ' ', i)
		nul := indexByte(data, 0, sp)
		mode := string(data[i:sp])
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		i = nul + 21
	}
	return entries
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func treeSortName(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func (s *Store) BuildTree(repo *Repo, entries []TreeEntry) string {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := treeSortName(sorted[i]), treeSortName(sorted[j])
		return a < b
	})
	var parts []byte
	for _, e := range sorted {
		parts = append(parts, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		parts = append(parts, raw...)
	}
	return s.PutObject(repo, ObjTree, parts)
}

// FlattenTree flattens a tree (by oid) to path -> {oid, mode}.
func (s *Store) FlattenTree(repo *Repo, treeOID string) map[string]TreeEntry {
	out := make(map[string]TreeEntry)
	if treeOID == "" {
		return out
	}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjTree {
			return
		}
		for _, e := range s.ParseTree(obj.Data) {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, p)
			} else {
				out[p] = e
			}
		}
	}
	walk(treeOID, "")
	return out
}

// BuildTreeFromFiles builds nested trees from a flat path map; returns root tree oid.
func (s *Store) BuildTreeFromFiles(repo *Repo, files map[string]TreeEntry) string {
	var build func(prefix string) string
	build = func(prefix string) string {
		entries := []TreeEntry{}
		dirs := make(map[string]bool)
		for path, v := range files {
			if prefix != "" && !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			if slash := strings.IndexByte(rest, '/'); slash == -1 {
				entries = append(entries, TreeEntry{Mode: v.Mode, Name: rest, OID: v.OID})
			} else {
				dirs[rest[:slash]] = true
			}
		}
		dirNames := make([]string, 0, len(dirs))
		for d := range dirs {
			dirNames = append(dirNames, d)
		}
		sort.Strings(dirNames)
		for _, dir := range dirNames {
			subPrefix := dir
			if prefix != "" {
				subPrefix = prefix + "/" + dir
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: build(subPrefix)})
		}
		return s.BuildTree(repo, entries)
	}
	return build("")
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func (s *Store) ParseCommit(data []byte) CommitInfo {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head := text
	message := ""
	if headEnd != -1 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	c := CommitInfo{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[10:]
		}
	}
	c.Message = message
	return c
}

// CreateCommit creates a commit object; returns its oid.
func (s *Store) CreateCommit(repo *Repo, treeOID string, parents []string, message string, epochSec int64) string {
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	ident := s.ViewerName + " <" + s.ViewerEmail + "> " + strconv.FormatInt(epochSec, 10) + " +0000"
	var sb strings.Builder
	sb.WriteString("tree " + treeOID + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + ident + "\ncommitter " + ident + "\n\n" + msg)
	return s.PutObject(repo, ObjCommit, []byte(sb.String()))
}

// ReachableCommits returns all commit oids reachable from roots.
func (s *Store) ReachableCommits(repo *Repo, roots []string) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		if obj.Type == ObjCommit {
			seen[oid] = true
			c := s.ParseCommit(obj.Data)
			stack = append(stack, c.Parents...)
		} else if obj.Type == ObjTag {
			if m := tagTarget(obj.Data); m != "" {
				stack = append(stack, m)
			}
		}
	}
	return seen
}

func tagTarget(data []byte) string {
	text := string(data)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

// ReachableObjects returns the full reachable object set from roots.
// stopAt (shallow boundaries): a commit in it contributes itself + its
// tree, but its parents are NOT walked.
func (s *Store) ReachableObjects(repo *Repo, roots []string, stopAt map[string]bool) map[string]*GitObject {
	out := make(map[string]*GitObject)
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := out[oid]; ok {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		out[oid] = obj
		switch obj.Type {
		case ObjCommit:
			c := s.ParseCommit(obj.Data)
			stack = append(stack, c.Tree)
			if !stopAt[oid] {
				stack = append(stack, c.Parents...)
			}
		case ObjTree:
			for _, e := range s.ParseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		case ObjTag:
			if m := tagTarget(obj.Data); m != "" {
				stack = append(stack, m)
			}
		}
	}
	return out
}

// IsAncestor reports whether ancestor is an ancestor of (or equal to) oid.
func (s *Store) IsAncestor(repo *Repo, ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := repo.Objects[cur]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		stack = append(stack, s.ParseCommit(obj.Data).Parents...)
	}
	return false
}

// MergeBase returns the first common ancestor of a and b (newest-first walk).
func (s *Store) MergeBase(repo *Repo, a, b string) string {
	aSet := s.ReachableCommits(repo, []string{a})
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if aSet[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := repo.Objects[cur]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		queue = append(queue, s.ParseCommit(obj.Data).Parents...)
	}
	return ""
}

// DiffFiles diffs two trees (flat maps) into a PullFile list.
func (s *Store) DiffFiles(oldFiles, newFiles map[string]TreeEntry, repo *Repo) []PullFile {
	out := []PullFile{}
	paths := make(map[string]bool)
	for p := range oldFiles {
		paths[p] = true
	}
	for p := range newFiles {
		paths[p] = true
	}
	// Deterministic order: sorted paths (Node sorts the result by filename).
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, path := range sorted {
		o, hasO := oldFiles[path]
		n, hasN := newFiles[path]
		if hasO && hasN && o.OID == n.OID {
			continue
		}
		oldText := ""
		if hasO {
			if obj := repo.Objects[o.OID]; obj != nil {
				oldText = string(obj.Data)
			}
		}
		newText := ""
		if hasN {
			if obj := repo.Objects[n.OID]; obj != nil {
				newText = string(obj.Data)
			}
		}
		oldLines := []string{}
		if oldText != "" {
			oldLines = strings.Split(oldText, "\n")
		}
		newLines := []string{}
		if newText != "" {
			newLines = strings.Split(newText, "\n")
		}
		oldSet := make(map[string]int)
		for _, l := range oldLines {
			oldSet[l]++
		}
		additions := 0
		for _, l := range newLines {
			if oldSet[l] > 0 {
				oldSet[l]--
			} else {
				additions++
			}
		}
		newSet := make(map[string]int)
		for _, l := range newLines {
			newSet[l]++
		}
		deletions := 0
		for _, l := range oldLines {
			if newSet[l] > 0 {
				newSet[l]--
			} else {
				deletions++
			}
		}
		status := "modified"
		if !hasO {
			status = "added"
		} else if !hasN {
			status = "removed"
		}
		out = append(out, PullFile{Filename: path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return out
}

type PullComputation struct {
	Files        []PullFile
	MergeBaseSha string
	Conflict     bool
}

// ComputePull computes PR files + mergeability from head/base tips.
func (s *Store) ComputePull(repo *Repo, headSha, baseSha string) PullComputation {
	mb := ""
	if baseSha != ZeroOID {
		mb = s.MergeBase(repo, headSha, baseSha)
	}
	var headTree string
	if headCommit := repo.Objects[headSha]; headCommit != nil && headCommit.Type == ObjCommit {
		headTree = s.ParseCommit(headCommit.Data).Tree
	}
	headFiles := s.FlattenTree(repo, headTree)
	mbFiles := make(map[string]TreeEntry)
	if mb != "" {
		if mbObj := repo.Objects[mb]; mbObj != nil {
			mbFiles = s.FlattenTree(repo, s.ParseCommit(mbObj.Data).Tree)
		}
	}
	files := s.DiffFiles(mbFiles, headFiles, repo)
	conflict := false
	if mb != "" {
		var baseTree string
		if baseCommit := repo.Objects[baseSha]; baseCommit != nil && baseCommit.Type == ObjCommit {
			baseTree = s.ParseCommit(baseCommit.Data).Tree
		}
		baseFiles := s.FlattenTree(repo, baseTree)
		for _, f := range files {
			mbV, hasMb := mbFiles[f.Filename]
			baseV, hasBase := baseFiles[f.Filename]
			headV, hasHead := headFiles[f.Filename]
			mbOID := ""
			if hasMb {
				mbOID = mbV.OID
			}
			baseOID := ""
			if hasBase {
				baseOID = baseV.OID
			}
			headOID := ""
			if hasHead {
				headOID = headV.OID
			}
			baseChanged := baseOID != mbOID
			headChanged := headOID != mbOID
			if baseChanged && headChanged && baseOID != headOID {
				conflict = true
				break
			}
		}
	}
	return PullComputation{Files: files, MergeBaseSha: mb, Conflict: conflict}
}
