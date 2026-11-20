package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ViewerInfo struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type LabelRecord struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type CommentRecord struct {
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Seq       int    `json:"-"`
}

type IssueRecord struct {
	ID        int           `json:"id"`
	NodeID    string        `json:"node_id"`
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	State     string        `json:"state"`
	Labels    []LabelRecord `json:"labels"`
	CreatedAt string        `json:"created_at"`
	Seq       int           `json:"-"`

	jsonBytes    []byte
	http200Frame []byte
	http201Frame []byte
}

type RefEnd struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type PullRecord struct {
	ID             int     `json:"id"`
	NodeID         string  `json:"node_id"`
	Number         int     `json:"number"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	Head           RefEnd  `json:"head"`
	Base           RefEnd  `json:"base"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	CreatedAt      string  `json:"created_at"`

	headRef string
	headSHA string
	baseRef string
	baseSHA string
}

type RepoOwner struct {
	Login string `json:"login"`
}

type RepoJSON struct {
	ID            int       `json:"id"`
	NodeID        string    `json:"node_id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     string    `json:"created_at"`
	Owner         RepoOwner `json:"owner"`
}

type CachedHTTPFrame struct {
	Version uint64
	Body    []byte
}

type RepoRecord struct {
	mu sync.RWMutex

	ID            int
	GlobalID      string
	Owner         string
	Name          string
	FullName      string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     string

	Objects       map[string]*GitObject
	CommitParents map[string][]string
	Refs          map[string]string
	RefVersion    uint64

	ReceiveRefsCache atomic.Pointer[CachedHTTPFrame]
	LsRefsCache      sync.Map
	ShallowCache     sync.Map
	HasLsRefsCache   bool
	HasShallowCache  bool

	Labels           map[string]LabelRecord
	NextNumber       int
	Issues           []*IssueRecord
	IssuesByNumber   map[int]*IssueRecord
	Pulls            []*PullRecord
	PullsByNumber    map[int]*PullRecord
	CommentsByNumber map[int][]*CommentRecord
}

func (r *RepoRecord) JSONView() RepoJSON {
	return RepoJSON{
		ID:            r.ID,
		NodeID:        r.GlobalID,
		Name:          r.Name,
		FullName:      r.FullName,
		Private:       r.Private,
		Description:   r.Description,
		DefaultBranch: r.DefaultBranch,
		CreatedAt:     r.CreatedAt,
		Owner:         RepoOwner{Login: r.Owner},
	}
}

type StoreError struct {
	Status  int
	Message string
}

func (e *StoreError) Error() string {
	return e.Message
}

func EncodeGlobalID(kind string, a int, b ...int) string {
	var raw string
	if len(b) == 0 {
		raw = kind + ":" + strconv.Itoa(a)
	} else {
		raw = kind + ":" + strconv.Itoa(a) + ":" + strconv.Itoa(b[0])
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeGlobalID(globalID string) (string, int, int, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(globalID)
	if err != nil {
		return "", 0, 0, false
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", 0, 0, false
	}
	a, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, 0, false
	}
	if len(parts) == 2 {
		return parts[0], a, 0, true
	}
	b, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, 0, false
	}
	return parts[0], a, b, true
}

func buildHTTPFrameBytes(statusCode int, statusText string, body []byte, extraHeaders string) []byte {
	var b strings.Builder
	b.Grow(160 + len(extraHeaders) + len(body))
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(statusCode))
	b.WriteByte(' ')
	b.WriteString(statusText)
	b.WriteString("\r\nContent-Type: application/json; charset=utf-8\r\nX-GitHub-Request-Id: fg-ok\r\n")
	if extraHeaders != "" {
		b.WriteString(extraHeaders)
	}
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(len(body)))
	b.WriteString("\r\n\r\n")
	out := make([]byte, b.Len()+len(body))
	copy(out, b.String())
	copy(out[b.Len():], body)
	return out
}

func appendEscapedJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '<':
			dst = append(dst, `\u003c`...)
		case '>':
			dst = append(dst, `\u003e`...)
		case '&':
			dst = append(dst, `\u0026`...)
		default:
			if c < 0x20 {
				dst = append(dst, `\u00`...)
				dst = append(dst, hexDigits[c>>4], hexDigits[c&0x0f])
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}

func rebuildIssueFrames(issue *IssueRecord) {
	var jb []byte
	if len(issue.Labels) == 0 {
		jb = make([]byte, 0, 160+len(issue.Title)+len(issue.Body))
		jb = append(jb, `{"id":`...)
		jb = strconv.AppendInt(jb, int64(issue.ID), 10)
		jb = append(jb, `,"node_id":`...)
		jb = appendEscapedJSONString(jb, issue.NodeID)
		jb = append(jb, `,"number":`...)
		jb = strconv.AppendInt(jb, int64(issue.Number), 10)
		jb = append(jb, `,"title":`...)
		jb = appendEscapedJSONString(jb, issue.Title)
		jb = append(jb, `,"body":`...)
		jb = appendEscapedJSONString(jb, issue.Body)
		jb = append(jb, `,"state":`...)
		jb = appendEscapedJSONString(jb, issue.State)
		jb = append(jb, `,"labels":[],"created_at":`...)
		jb = appendEscapedJSONString(jb, issue.CreatedAt)
		jb = append(jb, '}')
	} else {
		jb, _ = json.Marshal(issue)
	}
	issue.jsonBytes = jb
	issue.http200Frame = buildHTTPFrameBytes(200, "OK", jb, "")
	issue.http201Frame = buildHTTPFrameBytes(201, "Created", jb, "")
}

type ForgeStore struct {
	mu            sync.RWMutex
	Viewer        ViewerInfo
	baseEpochSec  int64
	clockSeq      atomic.Int64
	nextRepoID    atomic.Int64
	nextIssueID   atomic.Int64
	nextPullID    atomic.Int64
	nextCommentID atomic.Int64
	reposByName   map[string]*RepoRecord
	reposByID     map[int]*RepoRecord
}

func parseTrialEpochSec(rawNow string) int64 {
	if rawNow != "" {
		if t, err := time.Parse(time.RFC3339Nano, rawNow); err == nil {
			return t.Unix()
		}
	}
	return 1735689600
}

func NewForgeStore(viewerLogin, trialNow string) *ForgeStore {
	login := strings.TrimSpace(viewerLogin)
	if login == "" {
		login = "adaiversen"
	}
	name := login
	if login == "adaiversen" {
		name = "ada iversen"
	}
	st := &ForgeStore{
		Viewer: ViewerInfo{
			ID:    1000,
			Login: login,
			Name:  name,
			Email: login + "@example.invalid",
		},
		baseEpochSec: parseTrialEpochSec(trialNow),
		reposByName:  make(map[string]*RepoRecord, 64),
		reposByID:    make(map[int]*RepoRecord, 64),
	}
	st.nextRepoID.Store(50000)
	st.nextIssueID.Store(100000)
	st.nextPullID.Store(200000)
	st.nextCommentID.Store(300000)
	return st
}

func (s *ForgeStore) nextClock() (int64, string, int) {
	seq := s.clockSeq.Add(1)
	epoch := s.baseEpochSec + seq
	iso := time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	return epoch, iso, int(seq)
}

func (r *RepoRecord) SaveGitObjectLocked(obj *GitObject) *GitObject {
	if existing := r.Objects[obj.OID]; existing != nil {
		if existing.Deflated == nil && obj.Deflated != nil {
			existing.Deflated = obj.Deflated
		}
		return existing
	}
	r.Objects[obj.OID] = obj
	if obj.Type == "commit" {
		r.CommitParents[obj.OID] = ExtractCommitParents(obj.Data)
	}
	return obj
}

func (r *RepoRecord) BumpRefVersionLocked() {
	r.RefVersion++
	r.ReceiveRefsCache.Store(nil)
	if r.HasLsRefsCache {
		r.HasLsRefsCache = false
		r.LsRefsCache.Range(func(k, _ any) bool {
			r.LsRefsCache.Delete(k)
			return true
		})
	}
	if r.HasShallowCache {
		r.HasShallowCache = false
		r.ShallowCache.Range(func(k, _ any) bool {
			r.ShallowCache.Delete(k)
			return true
		})
	}
}

func (s *ForgeStore) GetRepo(owner, name string) *RepoRecord {
	if owner != s.Viewer.Login {
		return nil
	}
	s.mu.RLock()
	repo := s.reposByName[name]
	s.mu.RUnlock()
	return repo
}

func (s *ForgeStore) RequireRepo(owner, name string) (*RepoRecord, error) {
	repo := s.GetRepo(owner, name)
	if repo == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	return repo, nil
}

func (s *ForgeStore) GetRepoByGlobalID(gid string) *RepoRecord {
	kind, id, _, ok := DecodeGlobalID(gid)
	if !ok || kind != "Repo" {
		return nil
	}
	s.mu.RLock()
	repo := s.reposByID[id]
	s.mu.RUnlock()
	return repo
}

func (s *ForgeStore) GetIssueByGlobalID(gid string) (*RepoRecord, *IssueRecord) {
	kind, repoID, num, ok := DecodeGlobalID(gid)
	if !ok || kind != "Issue" {
		return nil, nil
	}
	s.mu.RLock()
	repo := s.reposByID[repoID]
	s.mu.RUnlock()
	if repo == nil {
		return nil, nil
	}
	repo.mu.RLock()
	iss := repo.IssuesByNumber[num]
	repo.mu.RUnlock()
	if iss == nil {
		return nil, nil
	}
	return repo, iss
}

func (s *ForgeStore) GetPullByGlobalID(gid string) (*RepoRecord, *PullRecord) {
	kind, repoID, num, ok := DecodeGlobalID(gid)
	if !ok || kind != "Pull" {
		return nil, nil
	}
	s.mu.RLock()
	repo := s.reposByID[repoID]
	s.mu.RUnlock()
	if repo == nil {
		return nil, nil
	}
	repo.mu.RLock()
	pr := repo.PullsByNumber[num]
	repo.mu.RUnlock()
	if pr == nil {
		return nil, nil
	}
	return repo, pr
}

func (s *ForgeStore) CreateRepo(name, desc string, private bool) (*RepoRecord, error) {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return nil, &StoreError{Status: 422, Message: "Repository name is required"}
	}
	id := int(s.nextRepoID.Add(1))
	gid := EncodeGlobalID("Repo", id)
	_, createdAt, _ := s.nextClock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reposByName[clean] != nil {
		return nil, &StoreError{Status: 422, Message: "Repository creation failed: name already exists on this account"}
	}
	repo := &RepoRecord{
		ID:               id,
		GlobalID:         gid,
		Owner:            s.Viewer.Login,
		Name:             clean,
		FullName:         s.Viewer.Login + "/" + clean,
		Private:          private,
		Description:      desc,
		DefaultBranch:    "main",
		CreatedAt:        createdAt,
		Objects:          make(map[string]*GitObject),
		CommitParents:    make(map[string][]string),
		Refs:             make(map[string]string),
		RefVersion:       1,
		Labels:           make(map[string]LabelRecord),
		NextNumber:       1,
		Issues:           make([]*IssueRecord, 0),
		IssuesByNumber:   make(map[int]*IssueRecord),
		Pulls:            make([]*PullRecord, 0),
		PullsByNumber:    make(map[int]*PullRecord),
		CommentsByNumber: make(map[int][]*CommentRecord),
	}
	s.reposByName[clean] = repo
	s.reposByID[id] = repo
	return repo, nil
}

func (s *ForgeStore) ListViewerRepos() []RepoJSON {
	s.mu.RLock()
	repos := make([]*RepoRecord, 0, len(s.reposByName))
	for _, r := range s.reposByName {
		repos = append(repos, r)
	}
	s.mu.RUnlock()
	sort.Slice(repos, func(i, j int) bool {
		return repos[i].ID < repos[j].ID
	})
	out := make([]RepoJSON, len(repos))
	for i, r := range repos {
		r.mu.RLock()
		out[i] = r.JSONView()
		r.mu.RUnlock()
	}
	return out
}

func (s *ForgeStore) PatchRepo(owner, name string, desc *string, private *bool, defBranch *string) (RepoJSON, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return RepoJSON{}, err
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if desc != nil {
		repo.Description = *desc
	}
	if private != nil {
		repo.Private = *private
	}
	if defBranch != nil && strings.TrimSpace(*defBranch) != "" {
		repo.DefaultBranch = strings.TrimSpace(*defBranch)
	}
	return repo.JSONView(), nil
}

func (s *ForgeStore) DeleteRepo(owner, name string) error {
	if owner != s.Viewer.Login {
		return &StoreError{Status: 404, Message: "Not Found"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.reposByName[name]
	if repo == nil {
		return &StoreError{Status: 404, Message: "Not Found"}
	}
	delete(s.reposByName, name)
	delete(s.reposByID, repo.ID)
	return nil
}

func (r *RepoRecord) resolveCommitLocked(refSpec string) string {
	target := strings.TrimSpace(refSpec)
	if target == "" {
		target = r.DefaultBranch
	}
	var candidates []string
	if strings.HasPrefix(target, "refs/") {
		candidates = []string{target}
	} else {
		candidates = []string{"refs/heads/" + target, "refs/tags/" + target}
	}
	for _, c := range candidates {
		if oid, ok := r.Refs[c]; ok {
			return PeelOID(r.Objects, oid)
		}
	}
	if IsValidOID(target) && r.Objects[target] != nil {
		return PeelOID(r.Objects, target)
	}
	return ""
}

func (s *ForgeStore) authorLine(epoch int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", s.Viewer.Name, s.Viewer.Email, epoch)
}

func (s *ForgeStore) PutContent(owner, name, rawPath, message, b64Content, branch, wantSHA string) (int, map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return 0, nil, err
	}
	cleanPath := strings.Trim(rawPath, "/")
	if cleanPath == "" {
		return 0, nil, &StoreError{Status: 422, Message: "Invalid file path"}
	}
	msg := message
	if msg == "" {
		msg = "Update " + cleanPath
	}

	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, b64Content)
	bytes, decErr := base64.StdEncoding.DecodeString(stripped)
	if decErr != nil {
		return 0, nil, &StoreError{Status: 422, Message: "Invalid base64 content"}
	}

	epoch, _, _ := s.nextClock()
	who := s.authorLine(epoch)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	targetBranch := strings.TrimSpace(branch)
	if targetBranch == "" {
		targetBranch = repo.DefaultBranch
	}
	refName := "refs/heads/" + targetBranch
	parentOID := repo.Refs[refName]
	if parentOID == "" && targetBranch != repo.DefaultBranch {
		parentOID = repo.Refs["refs/heads/"+repo.DefaultBranch]
	}

	var pathMap map[string]PathBlobEntry
	if parentOID != "" {
		pathMap = FlattenTree(repo.Objects, parentOID)
	} else {
		pathMap = make(map[string]PathBlobEntry)
	}

	existing, existed := pathMap[cleanPath]
	if wantSHA != "" {
		if !existed || existing.OID != strings.ToLower(wantSHA) {
			return 0, nil, &StoreError{Status: 409, Message: cleanPath + " does not match " + wantSHA}
		}
	}

	blobObj := repo.SaveGitObjectLocked(CreateGitObject("blob", bytes, nil))
	mode := "100644"
	if existed {
		mode = existing.Mode
	}
	pathMap[cleanPath] = PathBlobEntry{Mode: mode, OID: blobObj.OID}
	treeOID := BuildTreeFromPaths(repo.SaveGitObjectLocked, pathMap)

	var parents []string
	if parentOID != "" {
		parents = []string{parentOID}
	}
	commitObj := repo.SaveGitObjectLocked(CreateGitObject("commit", BuildCommit(treeOID, parents, who, who, msg), nil))
	repo.Refs[refName] = commitObj.OID
	repo.BumpRefVersionLocked()

	status := 201
	if existed {
		status = 200
	}
	baseName := cleanPath
	if idx := strings.LastIndexByte(cleanPath, '/'); idx >= 0 {
		baseName = cleanPath[idx+1:]
	}
	payload := map[string]any{
		"content": map[string]any{
			"name":     baseName,
			"path":     cleanPath,
			"sha":      blobObj.OID,
			"size":     len(bytes),
			"encoding": "base64",
			"type":     "file",
		},
		"commit": map[string]any{
			"sha":     commitObj.OID,
			"message": msg,
		},
	}
	return status, payload, nil
}

func (s *ForgeStore) GetContent(owner, name, rawPath, ref string) (any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	cleanPath := strings.Trim(rawPath, "/")

	repo.mu.RLock()
	defer repo.mu.RUnlock()

	commitOID := repo.resolveCommitLocked(ref)
	if commitOID == "" {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	cObj := repo.Objects[commitOID]
	if cObj == nil || cObj.Type != "commit" {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	curTreeOID := ParseCommit(cObj.Data).Tree

	dirListing := func(prefix, treeOID string) ([]map[string]any, error) {
		tObj := repo.Objects[treeOID]
		if tObj == nil || tObj.Type != "tree" {
			return nil, &StoreError{Status: 404, Message: "Not Found"}
		}
		entries := ParseTree(tObj.Data)
		out := make([]map[string]any, len(entries))
		for i, e := range entries {
			t := "file"
			sz := 0
			if e.Mode == "40000" {
				t = "dir"
			} else if b := repo.Objects[e.OID]; b != nil {
				sz = len(b.Data)
			}
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			out[i] = map[string]any{
				"type": t,
				"name": e.Name,
				"path": p,
				"sha":  e.OID,
				"size": sz,
			}
		}
		return out, nil
	}

	if cleanPath == "" {
		return dirListing("", curTreeOID)
	}

	parts := strings.Split(cleanPath, "/")
	for i, seg := range parts {
		tObj := repo.Objects[curTreeOID]
		if tObj == nil || tObj.Type != "tree" {
			return nil, &StoreError{Status: 404, Message: "Not Found"}
		}
		var matched *TreeEntry
		for _, e := range ParseTree(tObj.Data) {
			if e.Name == seg {
				eCopy := e
				matched = &eCopy
				break
			}
		}
		if matched == nil {
			return nil, &StoreError{Status: 404, Message: "Not Found"}
		}
		if i == len(parts)-1 {
			if matched.Mode == "40000" {
				return dirListing(cleanPath, matched.OID)
			}
			bObj := repo.Objects[matched.OID]
			if bObj == nil || bObj.Type != "blob" {
				return nil, &StoreError{Status: 404, Message: "Not Found"}
			}
			return map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString(bObj.Data),
				"sha":      bObj.OID,
				"size":     len(bObj.Data),
				"name":     seg,
				"path":     cleanPath,
			}, nil
		}
		if matched.Mode != "40000" {
			return nil, &StoreError{Status: 404, Message: "Not Found"}
		}
		curTreeOID = matched.OID
	}
	return nil, &StoreError{Status: 404, Message: "Not Found"}
}

func (s *ForgeStore) DeleteContent(owner, name, rawPath, message, wantSHA, branch string) (map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	cleanPath := strings.Trim(rawPath, "/")
	msg := message
	if msg == "" {
		msg = "Delete " + cleanPath
	}
	epoch, _, _ := s.nextClock()
	who := s.authorLine(epoch)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	targetBranch := strings.TrimSpace(branch)
	if targetBranch == "" {
		targetBranch = repo.DefaultBranch
	}
	refName := "refs/heads/" + targetBranch
	tipOID := repo.Refs[refName]
	if tipOID == "" {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	pathMap := FlattenTree(repo.Objects, tipOID)
	existing, ok := pathMap[cleanPath]
	if !ok {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	if wantSHA != "" && existing.OID != strings.ToLower(wantSHA) {
		return nil, &StoreError{Status: 409, Message: "sha does not match"}
	}
	delete(pathMap, cleanPath)
	treeOID := BuildTreeFromPaths(repo.SaveGitObjectLocked, pathMap)
	commitObj := repo.SaveGitObjectLocked(CreateGitObject("commit", BuildCommit(treeOID, []string{tipOID}, who, who, msg), nil))
	repo.Refs[refName] = commitObj.OID
	repo.BumpRefVersionLocked()

	return map[string]any{
		"commit": map[string]any{
			"sha":     commitObj.OID,
			"message": msg,
		},
	}, nil
}

func (s *ForgeStore) CreateRef(owner, name, ref, sha string) (map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	if ref == "" || sha == "" {
		return nil, &StoreError{Status: 422, Message: "ref and sha are required"}
	}
	refName := ref
	if !strings.HasPrefix(refName, "refs/") {
		refName = "refs/heads/" + refName
	}
	lowerSHA := strings.ToLower(sha)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if !IsValidOID(lowerSHA) || repo.Objects[lowerSHA] == nil {
		return nil, &StoreError{Status: 422, Message: "Object does not exist"}
	}
	if repo.Refs[refName] != "" {
		return nil, &StoreError{Status: 422, Message: "Reference already exists"}
	}
	repo.Refs[refName] = lowerSHA
	repo.BumpRefVersionLocked()
	return map[string]any{
		"ref": refName,
		"object": map[string]any{
			"sha":  lowerSHA,
			"type": "commit",
		},
	}, nil
}

func (s *ForgeStore) GetRef(owner, name, refPath string) (map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	fullRef := refPath
	if !strings.HasPrefix(fullRef, "refs/") {
		fullRef = "refs/" + fullRef
	}
	repo.mu.RLock()
	oid := repo.Refs[fullRef]
	repo.mu.RUnlock()
	if oid == "" {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	return map[string]any{
		"ref": fullRef,
		"object": map[string]any{
			"sha":  oid,
			"type": "commit",
		},
	}, nil
}

func (s *ForgeStore) ListBranches(owner, name string) ([]map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	def := repo.DefaultBranch
	type br struct {
		name string
		sha  string
	}
	list := make([]br, 0)
	for ref, oid := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			list = append(list, br{name: ref[len("refs/heads/"):], sha: oid})
		}
	}
	repo.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].name == def {
			return true
		}
		if list[j].name == def {
			return false
		}
		return list[i].name < list[j].name
	})
	out := make([]map[string]any, len(list))
	for i, b := range list {
		out[i] = map[string]any{
			"name":   b.name,
			"commit": map[string]any{"sha": b.sha},
		}
	}
	return out, nil
}

func (s *ForgeStore) GetBranch(owner, name, branch string) (map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	oid := repo.Refs["refs/heads/"+branch]
	repo.mu.RUnlock()
	if oid == "" {
		return nil, &StoreError{Status: 404, Message: "Branch not found"}
	}
	return map[string]any{
		"name":   branch,
		"commit": map[string]any{"sha": oid},
	}, nil
}

func (s *ForgeStore) ListCommits(owner, name, shaParam string) ([]map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	tipOID := repo.resolveCommitLocked(shaParam)
	if tipOID == "" {
		return []map[string]any{}, nil
	}
	out := make([]map[string]any, 0, 16)
	seen := make(map[string]bool)
	queue := []string{tipOID}
	for i := 0; i < len(queue) && len(out) < 100; i++ {
		oid := queue[i]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		cv := ParseCommit(obj.Data)
		out = append(out, map[string]any{
			"sha":    oid,
			"commit": map[string]any{"message": cv.Message},
		})
		for _, p := range cv.Parents {
			if !seen[p] {
				queue = append(queue, p)
			}
		}
	}
	return out, nil
}

func (s *ForgeStore) CreateLabel(owner, name, labelName, color string) (LabelRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return LabelRecord{}, err
	}
	clean := strings.TrimSpace(labelName)
	if clean == "" {
		return LabelRecord{}, &StoreError{Status: 422, Message: "Label name is required"}
	}
	c := strings.TrimSpace(color)
	if c == "" {
		c = "ededed"
	}
	lbl := LabelRecord{Name: clean, Color: c}
	repo.mu.Lock()
	repo.Labels[clean] = lbl
	repo.mu.Unlock()
	return lbl, nil
}

func (s *ForgeStore) ListLabels(owner, name string) ([]LabelRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	out := make([]LabelRecord, 0, len(repo.Labels))
	for _, lbl := range repo.Labels {
		out = append(out, lbl)
	}
	repo.mu.RUnlock()
	return out, nil
}

func (s *ForgeStore) CreateIssue(owner, name, title, body string, labelNames []string, checkLabels bool) (*IssueRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(title) == "" {
		return nil, &StoreError{Status: 422, Message: "Issue title is required"}
	}

	entityID := int(s.nextIssueID.Add(1))
	_, createdAt, seq := s.nextClock()

	repo.mu.Lock()
	labels := make([]LabelRecord, 0, len(labelNames))
	if checkLabels {
		for _, ln := range labelNames {
			found, ok := repo.Labels[ln]
			if !ok {
				repo.mu.Unlock()
				return nil, &StoreError{Status: 422, Message: fmt.Sprintf("Validation Failed: unknown label %q", ln)}
			}
			labels = append(labels, found)
		}
	}
	num := repo.NextNumber
	repo.NextNumber++
	gid := EncodeGlobalID("Issue", repo.ID, num)
	issue := &IssueRecord{
		ID:        entityID,
		NodeID:    gid,
		Number:    num,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		CreatedAt: createdAt,
		Seq:       seq,
	}
	rebuildIssueFrames(issue)
	repo.Issues = append(repo.Issues, issue)
	repo.IssuesByNumber[num] = issue
	repo.mu.Unlock()

	return issue, nil
}

func (s *ForgeStore) GetIssue(owner, name string, number int) (*IssueRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	issue := repo.IssuesByNumber[number]
	repo.mu.RUnlock()
	if issue == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	return issue, nil
}

func (s *ForgeStore) ListIssues(owner, name, state, direction string) ([]*IssueRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	st := state
	if st == "" {
		st = "open"
	}
	repo.mu.RLock()
	filtered := make([]*IssueRecord, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		if st == "all" || iss.State == st {
			filtered = append(filtered, iss)
		}
	}
	repo.mu.RUnlock()
	asc := direction == "asc"
	sort.Slice(filtered, func(i, j int) bool {
		if asc {
			return filtered[i].Seq < filtered[j].Seq
		}
		return filtered[i].Seq > filtered[j].Seq
	})
	return filtered, nil
}

func (s *ForgeStore) UpdateIssue(owner, name string, number int, state, title, body *string) (*IssueRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	issue := repo.IssuesByNumber[number]
	if issue == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	if state != nil && (*state == "open" || *state == "closed") {
		issue.State = *state
	}
	if title != nil {
		issue.Title = *title
	}
	if body != nil {
		issue.Body = *body
	}
	rebuildIssueFrames(issue)
	return issue, nil
}

func (s *ForgeStore) AddComment(owner, name string, number int, body string) (*CommentRecord, error) {
	if body == "" {
		return nil, &StoreError{Status: 422, Message: "Comment body is required"}
	}
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}

	id := int(s.nextCommentID.Add(1))
	_, createdAt, seq := s.nextClock()

	gid := EncodeGlobalID("Comment", id)
	rec := &CommentRecord{
		ID:        id,
		NodeID:    gid,
		Body:      body,
		CreatedAt: createdAt,
		Seq:       seq,
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.IssuesByNumber[number] == nil && repo.PullsByNumber[number] == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	repo.CommentsByNumber[number] = append(repo.CommentsByNumber[number], rec)
	return rec, nil
}

func (s *ForgeStore) ListComments(owner, name string, number int) ([]*CommentRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if repo.IssuesByNumber[number] == nil && repo.PullsByNumber[number] == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	list := repo.CommentsByNumber[number]
	if list == nil {
		return []*CommentRecord{}, nil
	}
	out := make([]*CommentRecord, len(list))
	copy(out, list)
	return out, nil
}

func (s *ForgeStore) CreatePull(owner, name, title, body, head, base string) (*PullRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	if title == "" || head == "" || base == "" {
		return nil, &StoreError{Status: 422, Message: "title, head, and base are required"}
	}
	headRef := strings.TrimPrefix(head, "refs/heads/")
	baseRef := strings.TrimPrefix(base, "refs/heads/")

	id := int(s.nextPullID.Add(1))
	_, createdAt, _ := s.nextClock()

	repo.mu.Lock()
	headSHA := repo.Refs["refs/heads/"+headRef]
	baseSHA := repo.Refs["refs/heads/"+baseRef]
	if headSHA == "" || baseSHA == "" {
		repo.mu.Unlock()
		return nil, &StoreError{Status: 422, Message: "head or base branch does not exist"}
	}
	num := repo.NextNumber
	repo.NextNumber++
	gid := EncodeGlobalID("Pull", repo.ID, num)
	pr := &PullRecord{
		ID:        id,
		NodeID:    gid,
		Number:    num,
		State:     "open",
		Title:     title,
		Body:      body,
		Head:      RefEnd{Ref: headRef, SHA: headSHA},
		Base:      RefEnd{Ref: baseRef, SHA: baseSHA},
		Merged:    false,
		CreatedAt: createdAt,
		headRef:   headRef,
		headSHA:   headSHA,
		baseRef:   baseRef,
		baseSHA:   baseSHA,
	}
	repo.Pulls = append(repo.Pulls, pr)
	repo.PullsByNumber[num] = pr
	repo.mu.Unlock()

	return pr, nil
}

func clonePullSnapshot(pr *PullRecord) *PullRecord {
	cp := *pr
	return &cp
}

func (s *ForgeStore) ListPulls(owner, name, state string) ([]*PullRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	st := state
	if st == "" {
		st = "open"
	}
	repo.mu.RLock()
	out := make([]*PullRecord, 0, len(repo.Pulls))
	for _, pr := range repo.Pulls {
		if st == "all" || pr.State == st {
			out = append(out, clonePullSnapshot(pr))
		}
	}
	repo.mu.RUnlock()
	return out, nil
}

func (s *ForgeStore) GetPull(owner, name string, number int) (*PullRecord, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	pr := repo.PullsByNumber[number]
	if pr == nil {
		repo.mu.RUnlock()
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	cp := clonePullSnapshot(pr)
	repo.mu.RUnlock()
	return cp, nil
}

func (s *ForgeStore) GetPullFiles(owner, name string, number int) ([]FileDiffItem, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	pr := repo.PullsByNumber[number]
	if pr == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	curHead := repo.Refs["refs/heads/"+pr.headRef]
	if curHead == "" {
		curHead = pr.headSHA
	}
	curBase := repo.Refs["refs/heads/"+pr.baseRef]
	if curBase == "" {
		curBase = pr.baseSHA
	}
	ancOID := FindMergeBase(repo.CommitParents, curBase, curHead)
	if ancOID == "" {
		ancOID = pr.baseSHA
	}
	ancMap := FlattenTree(repo.Objects, ancOID)
	headMap := FlattenTree(repo.Objects, curHead)
	return DiffTrees(repo.Objects, ancMap, headMap), nil
}

func (s *ForgeStore) IsPullMergeable(repo *RepoRecord, pr *PullRecord) bool {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	_, _, _, ok := s.computePullMergeLocked(repo, pr)
	return ok
}

func (s *ForgeStore) computePullMergeLocked(repo *RepoRecord, pr *PullRecord) (string, string, string, bool) {
	baseTip := repo.Refs["refs/heads/"+pr.baseRef]
	if baseTip == "" {
		baseTip = pr.baseSHA
	}
	headTip := repo.Refs["refs/heads/"+pr.headRef]
	if headTip == "" {
		headTip = pr.headSHA
	}
	ancOID := FindMergeBase(repo.CommitParents, baseTip, headTip)
	var ancMap map[string]PathBlobEntry
	if ancOID != "" {
		ancMap = FlattenTree(repo.Objects, ancOID)
	} else {
		ancMap = make(map[string]PathBlobEntry)
	}
	baseMap := FlattenTree(repo.Objects, baseTip)
	headMap := FlattenTree(repo.Objects, headTip)
	treeOID, ok := ThreeWayMergeTrees(repo.SaveGitObjectLocked, ancMap, baseMap, headMap)
	if !ok {
		return "", "", "", false
	}
	return treeOID, baseTip, headTip, true
}

func (s *ForgeStore) MergePull(owner, name string, number int, mergeMethod string) (map[string]any, error) {
	repo, err := s.RequireRepo(owner, name)
	if err != nil {
		return nil, err
	}
	epoch, iso, _ := s.nextClock()
	who := s.authorLine(epoch)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	pr := repo.PullsByNumber[number]
	if pr == nil {
		return nil, &StoreError{Status: 404, Message: "Not Found"}
	}
	if pr.Merged {
		return nil, &StoreError{Status: 405, Message: "Pull Request is already merged"}
	}
	treeOID, baseTip, headTip, ok := s.computePullMergeLocked(repo, pr)
	if !ok {
		return nil, &StoreError{Status: 405, Message: "Pull Request is not mergeable"}
	}
	parents := []string{baseTip}
	if strings.EqualFold(mergeMethod, "merge") {
		parents = []string{baseTip, headTip}
	}
	commitMsg := fmt.Sprintf("Merge pull request #%d: %s", pr.Number, pr.Title)
	commitObj := repo.SaveGitObjectLocked(CreateGitObject("commit", BuildCommit(treeOID, parents, who, who, commitMsg), nil))
	repo.Refs["refs/heads/"+pr.baseRef] = commitObj.OID
	repo.BumpRefVersionLocked()

	pr.baseSHA = commitObj.OID
	pr.Base.SHA = commitObj.OID
	pr.State = "closed"
	pr.Merged = true
	mergedAt := iso
	mergeSHA := commitObj.OID
	pr.MergedAt = &mergedAt
	pr.MergeCommitSHA = &mergeSHA

	return map[string]any{
		"sha":     commitObj.OID,
		"merged":  true,
		"message": "Pull Request successfully merged",
	}, nil
}
