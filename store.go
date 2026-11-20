// The forge's whole store: repositories, git objects, issues, pull requests,
// labels and comments, all in memory. Nothing here reads a wall clock or
// randomness: every timestamp comes from a deterministic sequence counter
// (seq), so two runs fed the same requests in the same order produce the
// same state, byte for byte. Port of store.ts.
package main

import (
	"sync"
	"sync/atomic"
	"time"
)

const epochSec = 1735689600

const viewerLogin = "adaiversen"
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"
const viewerID = 1000

type gitObjectType string

const (
	objCommit gitObjectType = "commit"
	objTree   gitObjectType = "tree"
	objBlob   gitObjectType = "blob"
	objTag    gitObjectType = "tag"
)

// GitObject is one git object. RawZlib holds the object's zlib stream exactly
// as received in a pack (for zero-deflate pack emission); Data is the
// inflated payload. Either may be empty for locally created objects
// (RawZlib is filled lazily on first pack write).
type GitObject struct {
	Type    gitObjectType
	Data    []byte
	RawZlib []byte
}

type Label struct {
	Name        string
	Color       string
	Description string
}

type IssueComment struct {
	ID         int
	Body       string
	User       string
	CreatedSeq int64
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	User       string
	CreatedSeq int64
	UpdatedSeq int64
	Comments   []*IssueComment
	// Cached single-issue GET body (JSON bytes) for cachedHost; cleared on
	// any mutation of the issue.
	cachedBody []byte
	cachedHost string
}

type PullRequest struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Head           string
	Base           string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       string // "" means null
	MergeCommitSha string // "" means null
	User           string
	CreatedSeq     int64
	UpdatedSeq     int64
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int64
	Objects       map[string]*GitObject
	Refs          map[string]string
	Labels        map[string]*Label
	Issues        map[int]*Issue
	IssueOrder    []*Issue // insertion order (== number order)
	Pulls         map[int]*PullRequest
	PullOrder     []*PullRequest
	NextNumber    int
	NextCommentID int
	FetchCache    map[string][]byte
	AdvCache      map[string][]byte
	LsRefsCache   map[string][]byte
	// Clone fast path: cached depth-1 fetch response for the current tip.
	cloneTip  string
	cloneResp atomic.Pointer[[]byte]
	// Receive-pack advertisement ping-pong: template built once, OID bytes
	// patched in place when only the tip moved (same ref names).
	recvAdvBuf  [2][]byte
	recvAdvFlip int
	recvAdvRef  string // ref-name fingerprint the template was built for
	recvAdvOffs []advOff
	recvAdvOID  string // OID currently in the template
}

type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo
	nextRepoID int
	seq       int64
	isoCache  map[int64]string
	// lastRepo single-entry cache (hot path).
	lastRepo atomic.Pointer[Repo]
}

func NewStore() *Store {
	return &Store{
		repos:      make(map[string]*Repo),
		nextRepoID: 1,
		isoCache:   make(map[int64]string),
	}
}

func (s *Store) key(owner, name string) string { return owner + "/" + name }

func (s *Store) GetRepo(owner, name string) *Repo {
	if r := s.lastRepo.Load(); r != nil && r.Owner == owner && r.Name == name {
		return r
	}
	s.mu.RLock()
	r := s.repos[s.key(owner, name)]
	s.mu.RUnlock()
	if r != nil {
		s.lastRepo.Store(r)
	}
	return r
}

// getRepoLocked returns the repo; caller must hold at least a read lock.
func (s *Store) getRepoLocked(owner, name string) *Repo {
	return s.repos[s.key(owner, name)]
}

// Tick returns the next deterministic timestamp, as epoch seconds.
func (s *Store) Tick() int64 {
	s.mu.Lock()
	s.seq++
	v := epochSec + s.seq
	s.mu.Unlock()
	return v
}

// tickLocked: caller holds the write lock.
func (s *Store) tickLocked() int64 {
	s.seq++
	return epochSec + s.seq
}

func (s *Store) IsoOf(epoch int64) string {
	s.mu.RLock()
	if v, ok := s.isoCache[epoch]; ok {
		s.mu.RUnlock()
		return v
	}
	s.mu.RUnlock()
	v := time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
	s.mu.Lock()
	if len(s.isoCache) > 200000 {
		s.isoCache = make(map[int64]string)
	}
	s.isoCache[epoch] = v
	s.mu.Unlock()
	return v
}

// isoOfLocked: caller holds at least a read lock; cache writes take the
// write lock only on a miss (called rarely per distinct second).
func (s *Store) isoOfLocked(epoch int64) string {
	if v, ok := s.isoCache[epoch]; ok {
		return v
	}
	v := time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
	// Best effort: cache it without upgrading the lock here; the caller
	// (write paths) stores it, read paths re-derive. To keep it simple and
	// race-free we store through a tiny separate mutex-free path: the map is
	// only ever written under s.mu write lock, so do it lazily in IsoOf.
	// Here we just return the computed value.
	return v
}

func (s *Store) CreateRepo(owner, name, description string, isPrivate bool) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createRepoLocked(owner, name, description, isPrivate)
}

// createRepoLocked: caller holds the write lock.
func (s *Store) createRepoLocked(owner, name, description string, isPrivate bool) *Repo {
	id := s.nextRepoID
	s.nextRepoID++
	s.seq++
	repo := &Repo{
		ID:            id,
		Name:          name,
		Owner:         owner,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		CreatedSeq:    s.seq,
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*PullRequest),
		NextNumber:    1,
		NextCommentID: 1,
		FetchCache:    make(map[string][]byte),
		AdvCache:      make(map[string][]byte),
		LsRefsCache:   make(map[string][]byte),
	}
	s.repos[s.key(owner, name)] = repo
	s.lastRepo.Store(repo)
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.repos[s.key(owner, name)]
	if ok {
		delete(s.repos, s.key(owner, name))
		if r := s.lastRepo.Load(); r != nil && r.Owner == owner && r.Name == name {
			s.lastRepo.Store(nil)
		}
	}
	return ok
}

func (s *Store) ListViewerRepos() []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listViewerReposLocked()
}

// listViewerReposLocked: caller holds a lock.
func (s *Store) listViewerReposLocked() []*Repo {
	var out []*Repo
	for _, r := range s.repos {
		if r.Owner == viewerLogin {
			out = append(out, r)
		}
	}
	// sort by id ascending (insertion sort is fine; few repos)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// repoByIDLocked finds a repo by its numeric id; caller holds a lock.
func (s *Store) repoByIDLocked(id int) *Repo {
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func clearIssueCache(issue *Issue) {
	issue.cachedBody = nil
	issue.cachedHost = ""
}

func clearAllIssueCaches(repo *Repo) {
	for _, issue := range repo.Issues {
		clearIssueCache(issue)
	}
}
