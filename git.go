// git, standard library only: object hashing, tree/commit/tag parsing and
// building, packfile read (plain + OFS/REF deltas) and write (plain objects,
// reusing stored RawZlib streams), reachability, fast-forward checks,
// merge-base, shallow boundaries, and the smart-HTTP protocol (v2 ls-refs /
// fetch, v0 upload-pack / receive-pack) as pkt-line streams. Port of git.ts.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

func isOID(v string) bool {
	if len(v) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hashObject(typ gitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// hashObjectBytes hashes without allocating the header string.
func hashObjectBytes(typ gitObjectType, data []byte) string {
	var hdr [32]byte
	n := copy(hdr[:], string(typ))
	hdr[n] = ' '
	n++
	n += copy(hdr[n:], strconvItoa(len(data)))
	hdr[n] = 0
	n++
	h := sha1.New()
	h.Write(hdr[:n])
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func strconvItoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func putObject(repo *Repo, typ gitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

// ---------------------------------------------------------------------------
// trees, commits, tags
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, errors.New("malformed tree object")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("malformed tree object")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("malformed tree object")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var message string
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	var cv CommitView
	cv.Message = message
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.IndexByte(line, ' ')
		key, value := line, ""
		if space >= 0 {
			key, value = line[:space], line[space+1:]
		}
		switch key {
		case "tree":
			cv.Tree = value
		case "parent":
			cv.Parents = append(cv.Parents, value)
		case "author":
			cv.Author = value
		case "committer":
			cv.Committer = value
		}
	}
	return cv
}

func buildCommitText(tree string, parents []string, message string, timestamp int64) []byte {
	who := fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, timestamp)
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + who + "\n")
	sb.WriteString("committer " + who + "\n")
	sb.WriteString("\n")
	sb.WriteString(message)
	return []byte(sb.String())
}

type TagView struct {
	Object  string
	Type    gitObjectType
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := make(map[string]string)
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.IndexByte(line, ' ')
		if space >= 0 {
			headers[line[:space]] = line[space+1:]
		}
	}
	t := TagView{
		Object: headers["object"],
		Tag:    headers["tag"],
		Tagger: headers["tagger"],
	}
	if v, ok := headers["type"]; ok {
		t.Type = gitObjectType(v)
	} else {
		t.Type = objCommit
	}
	if blank >= 0 {
		t.Message = string(data[blank+2:])
	}
	return t
}

func peelToCommit(repo *Repo, oid string) string {
	current := oid
	for hops := 0; hops < 16; hops++ {
		obj := repo.Objects[current]
		if obj == nil {
			return ""
		}
		if obj.Type == objCommit {
			return current
		}
		if obj.Type == objTag {
			current = parseTag(obj.Data).Object
			continue
		}
		return ""
	}
	return ""
}

// ---------------------------------------------------------------------------
// flat file maps
// ---------------------------------------------------------------------------

type FlatFile struct {
	Mode string
	OID  string
}

func flattenTree(repo *Repo, rootOID string) map[string]FlatFile {
	out := make(map[string]FlatFile)
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		obj := repo.Objects[treeOID]
		if obj == nil || obj.Type != objTree {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = FlatFile{Mode: e.Mode, OID: e.OID}
			}
		}
	}
	walk(rootOID, "")
	return out
}

func buildTreeFromFlat(repo *Repo, files map[string]FlatFile) string {
	type Node struct {
		files map[string]FlatFile
		dirs  map[string]*Node
	}
	mk := func() *Node { return &Node{files: make(map[string]FlatFile), dirs: make(map[string]*Node)} }
	root := mk()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file := files[path]
		segs := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segs)-1; i++ {
			child, ok := node.dirs[segs[i]]
			if !ok {
				child = mk()
				node.dirs[segs[i]] = child
			}
			node = child
		}
		node.files[segs[len(segs)-1]] = file
	}
	var build func(n *Node) string
	build = func(n *Node) string {
		var entries []TreeEntry
		for name, f := range n.files {
			entries = append(entries, TreeEntry{Mode: f.Mode, Name: name, OID: f.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return putObject(repo, objTree, buildTree(entries))
	}
	return build(root)
}

type FileChange struct {
	Path   string
	Status string // added | modified | removed
	OldOID string
	NewOID string
}

func diffFlat(oldMap, newMap map[string]FlatFile) []FileChange {
	pathsSet := make(map[string]struct{})
	for p := range oldMap {
		pathsSet[p] = struct{}{}
	}
	for p := range newMap {
		pathsSet[p] = struct{}{}
	}
	paths := make([]string, 0, len(pathsSet))
	for p := range pathsSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes []FileChange
	for _, path := range paths {
		oldEntry, hasOld := oldMap[path]
		newEntry, hasNew := newMap[path]
		switch {
		case !hasOld && hasNew:
			changes = append(changes, FileChange{Path: path, Status: "added", NewOID: newEntry.OID})
		case hasOld && !hasNew:
			changes = append(changes, FileChange{Path: path, Status: "removed", OldOID: oldEntry.OID})
		case hasOld && hasNew && oldEntry.OID != newEntry.OID:
			changes = append(changes, FileChange{Path: path, Status: "modified", OldOID: oldEntry.OID, NewOID: newEntry.OID})
		}
	}
	return changes
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

func collectReachable(roots []string, lookup func(string) *GitObject) map[string]struct{} {
	seen := make(map[string]struct{})
	var queue []string
	for _, r := range roots {
		if isOID(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = struct{}{}
		switch obj.Type {
		case objCommit:
			c := parseCommit(obj.Data)
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case objTag:
			queue = append(queue, parseTag(obj.Data).Object)
		case objTree:
			entries, err := parseTree(obj.Data)
			if err == nil {
				for _, e := range entries {
					if e.Mode != "160000" {
						queue = append(queue, e.OID)
					}
				}
			}
		}
	}
	return seen
}

func collectHaveReachable(roots []string, stopAt []string, lookup func(string) *GitObject) map[string]struct{} {
	stop := make(map[string]struct{})
	for _, s := range stopAt {
		stop[s] = struct{}{}
	}
	seen := make(map[string]struct{})
	var queue []string
	for _, r := range roots {
		if isOID(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = struct{}{}
		switch obj.Type {
		case objCommit:
			c := parseCommit(obj.Data)
			for extra := range collectReachable([]string{c.Tree}, lookup) {
				seen[extra] = struct{}{}
			}
			if _, isStop := stop[oid]; !isStop {
				queue = append(queue, c.Parents...)
			}
		case objTag:
			queue = append(queue, parseTag(obj.Data).Object)
		case objTree:
			entries, err := parseTree(obj.Data)
			if err == nil {
				for _, e := range entries {
					if e.Mode != "160000" {
						queue = append(queue, e.OID)
					}
				}
			}
		}
	}
	return seen
}

func isAncestor(repo *Repo, ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := make(map[string]struct{})
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		seen[oid] = struct{}{}
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

func mergeBase(repo *Repo, a, b string) string {
	ancestorsA := make(map[string]int)
	queueA := [][2]interface{}{{a, 0}}
	type item struct {
		oid   string
		depth int
	}
	qa := []item{{a, 0}}
	for len(qa) > 0 {
		cur := qa[0]
		qa = qa[1:]
		if _, ok := ancestorsA[cur.oid]; ok {
			continue
		}
		ancestorsA[cur.oid] = cur.depth
		obj := repo.Objects[cur.oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			qa = append(qa, item{p, cur.depth + 1})
		}
	}
	_ = queueA
	best := ""
	bestDepth := int(^uint(0) >> 1)
	qb := []item{{b, 0}}
	seenB := make(map[string]struct{})
	for len(qb) > 0 {
		cur := qb[0]
		qb = qb[1:]
		if _, ok := seenB[cur.oid]; ok {
			continue
		}
		seenB[cur.oid] = struct{}{}
		if dA, ok := ancestorsA[cur.oid]; ok && dA+cur.depth < bestDepth {
			bestDepth = dA + cur.depth
			best = cur.oid
		}
		obj := repo.Objects[cur.oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			qb = append(qb, item{p, cur.depth + 1})
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------

var typeCode = map[gitObjectType]int{objCommit: 1, objTree: 2, objBlob: 3, objTag: 4}
var codeType = map[int]gitObjectType{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}

// pooled zlib writers (BestSpeed == level 1, as Node's writePack).
var zlibWriterPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

func compressZlib(dst *bytes.Buffer, data []byte) {
	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(dst)
	_, _ = w.Write(data)
	_ = w.Close()
	zlibWriterPool.Put(w)
}

func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack truncated")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, offset, first, nil
}

func readOfsBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("pack truncated")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("pack truncated")
		}
		b = buf[offset]
		offset++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, offset, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) || written+copySize > targetSize {
				return nil, errors.New("delta copy out of range")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) || written+literal > targetSize {
				return nil, errors.New("delta truncated")
			}
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawPackEntry struct {
	offset    int
	typeCode  int
	size      int
	data      []byte
	rawZlib   []byte
	baseOff   int
	baseOID   string
	resolved  *resolvedObj
}

type resolvedObj struct {
	typ  gitObjectType
	data []byte
	oid  string
}

// readPack parses a packfile; returns every resolved object keyed by id.
// RawZlib sub-slices share the pack's bytes (zero-deflate on re-emission).
// extern supplies objects already known to the receiver (the repo's store):
// thin packs delta against such objects, so a REF_DELTA base may live
// outside the pack. Entries are resolved in passes so a REF_DELTA base
// that appears later in the pack (possibly itself a delta) also resolves.
//
// The returned objects keep RawZlib sub-slices of the pack for zero-deflate
// re-emission, so readPack CLONES the input first: callers hand us a slice
// of a request body buffer that the server reuses across keep-alive
// requests, and aliasing it would let a later request corrupt the stored
// zlib streams (kit run 1: 6 pack failures, "incorrect header check" and
// "declares 330 but inflated to 261" on re-emitted packs).
func readPack(pack []byte, extern map[string]*GitObject) (map[string]*GitObject, error) {
	pack = bytes.Clone(pack)
	if len(pack) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack header missing")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	var raw []*rawPackEntry
	byOffset := make(map[int]*rawPackEntry)
	offset := 12
	for i := 0; i < count; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		tc := int((first >> 4) & 0x07)
		entry := &rawPackEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, nxt, err := readOfsBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = nxt
			entry.baseOff = entryOffset - base
		} else if tc == 7 {
			if offset+20 > len(pack) {
				return nil, errors.New("pack truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("unknown pack type %d", tc)
		}
		// Inflate exactly one zlib stream. bytes.Reader implements
		// io.ByteReader, so flate reads byte-at-a-time with no over-read
		// and br.Len() tells us exactly how many bytes the stream took.
		br := bytes.NewReader(pack[offset:trailerStart])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		consumed := (trailerStart - offset) - br.Len()
		entry.data = data
		entry.rawZlib = pack[offset : offset+consumed]
		offset += consumed
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	objects := make(map[string]*GitObject)
	resolvedByOID := make(map[string]*rawPackEntry)
	resolving := make(map[int]bool)
	var resolve func(e *rawPackEntry) (*resolvedObj, error)
	resolve = func(e *rawPackEntry) (*resolvedObj, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		if resolving[e.offset] {
			return nil, errors.New("delta cycle")
		}
		resolving[e.offset] = true
		// Clear the mark on every exit (success or deferral) so a
		// later retry pass is not mistaken for a cycle.
		defer delete(resolving, e.offset)
		var typ gitObjectType
		var data []byte
		switch e.typeCode {
		case 6:
			baseEntry := byOffset[e.baseOff]
			if baseEntry == nil {
				return nil, errors.New("ofs-delta base missing")
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			typ = base.typ
			data, err = applyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
		case 7:
			baseEntry := resolvedByOID[e.baseOID]
			if baseEntry != nil {
				base, err := resolve(baseEntry)
				if err != nil {
					return nil, err
				}
				typ = base.typ
				data, err = applyDelta(base.data, e.data)
				if err != nil {
					return nil, err
				}
			} else if ext, ok := extern[e.baseOID]; ok {
				// Thin pack: delta base is an object the receiver
				// already has; it is a whole object, never a delta.
				typ = ext.Type
				var err error
				data, err = applyDelta(ext.Data, e.data)
				if err != nil {
					return nil, err
				}
			} else {
				// Base not resolved yet: it may be a later entry in
				// this pack (possibly itself a delta). Defer.
				return nil, errBaseNotReady
			}
		default:
			typ = codeType[e.typeCode]
			data = e.data
		}
		oid := hashObject(typ, data)
		e.resolved = &resolvedObj{typ: typ, data: data, oid: oid}
		resolvedByOID[oid] = e
		return e.resolved, nil
	}
	// Resolve in passes: a REF_DELTA base may be a later entry in this
	// pack (possibly itself a delta), so a deferred entry is retried
	// after the other entries have had a chance to resolve.
	for {
		progress := false
		deferred := false
		for _, e := range raw {
			if e.resolved != nil {
				continue
			}
			if _, err := resolve(e); err == errBaseNotReady {
				deferred = true
				continue
			} else if err != nil {
				return nil, err
			}
			progress = true
		}
		done := true
		for _, e := range raw {
			if e.resolved == nil {
				done = false
				break
			}
		}
		if done {
			break
		}
		if !progress {
			if deferred {
				return nil, errors.New("ref-delta base missing")
			}
			return nil, errors.New("pack entries could not be resolved")
		}
	}
	for _, e := range raw {
		r := e.resolved
		if _, ok := objects[r.oid]; !ok {
			obj := &GitObject{Type: r.typ, Data: r.data}
			// Keep the original zlib stream only for plain entries: a
			// delta-resolved object's bytes were never compressed as such.
			if e.typeCode != 6 && e.typeCode != 7 {
				obj.RawZlib = e.rawZlib
			}
			objects[r.oid] = obj
		}
	}
	return objects, nil
}

var errBaseNotReady = errors.New("ref-delta base not yet resolved")

func writePackObjectHeader(typ gitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining /= 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	out = append(out, first)
	return out
}

// writePack builds a packfile from plain objects (no deltas): header, zlib
// entries, SHA-1 trailer. Objects carrying RawZlib reuse those bytes
// (zero-deflate); others are deflated at level 1.
func writePack(objects []*GitObject) []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2})
	body.Write([]byte{byte(len(objects) >> 24), byte(len(objects) >> 16), byte(len(objects) >> 8), byte(len(objects))})
	for _, obj := range objects {
		body.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		if len(obj.RawZlib) > 0 {
			body.Write(obj.RawZlib)
		} else {
			compressZlib(&body, obj.Data)
		}
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// ---------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	n := len(payload) + 4
	out[0] = "0123456789abcdef"[n>>12&0xf]
	out[1] = "0123456789abcdef"[n>>8&0xf]
	out[2] = "0123456789abcdef"[n>>4&0xf]
	out[3] = "0123456789abcdef"[n&0xf]
	copy(out[4:], payload)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlushBytes = []byte("0000")
var pktDelimBytes = []byte("0001")

// pktLines encodes items; "flush" and "delim" are control items.
func pktLines(items []interface{}) []byte {
	var out []byte
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out = append(out, pktFlushBytes...)
			} else if v == "delim" {
				out = append(out, pktDelimBytes...)
			} else {
				out = append(out, pktLineStr(v)...)
			}
		case []byte:
			out = append(out, pktLine(v)...)
		}
	}
	return out
}

type Packet struct {
	Kind    string // "data" | "flush" | "delim" | "response-end"
	Payload []byte
}

func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func decodePktLines(data []byte) ([]Packet, error) {
	var packets []Packet
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			return nil, errors.New("truncated pkt-line")
		}
		header := data[offset : offset+4]
		if !isHex4(header) {
			return nil, errors.New("invalid pkt-line header")
		}
		length := 0
		for _, c := range header {
			length *= 16
			switch {
			case c >= '0' && c <= '9':
				length += int(c - '0')
			case c >= 'a' && c <= 'f':
				length += int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				length += int(c-'A') + 10
			}
		}
		switch length {
		case 0:
			packets = append(packets, Packet{Kind: "flush"})
			offset += 4
		case 1:
			packets = append(packets, Packet{Kind: "delim"})
			offset += 4
		case 2:
			packets = append(packets, Packet{Kind: "response-end"})
			offset += 4
		default:
			if offset+length > len(data) {
				return nil, errors.New("truncated pkt-line")
			}
			packets = append(packets, Packet{Kind: "data", Payload: data[offset+4 : offset+length]})
			offset += length
		}
	}
	return packets, nil
}

func sidebandWrap(data []byte, chunkSize int) []byte {
	if chunkSize <= 0 {
		chunkSize = 32768
	}
	var out []byte
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		payload := make([]byte, 1+len(chunk))
		payload[0] = 1
		copy(payload[1:], chunk)
		out = append(out, pktLine(payload)...)
	}
	if len(data) == 0 {
		out = append(out, pktLine([]byte{1})...)
	}
	return out
}

// ---------------------------------------------------------------------------
// ref helpers
// ---------------------------------------------------------------------------

func defaultBranchRef(repo *Repo) string { return "refs/heads/" + repo.DefaultBranch }

func headTarget(repo *Repo) (string, bool) {
	oid, ok := repo.Refs[defaultBranchRef(repo)]
	return oid, ok
}

type refPair struct {
	name string
	oid  string
}

func sortedRefs(repo *Repo) []refPair {
	out := make([]refPair, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, refPair{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

var uploadV2Caps = []string{"ls-refs=unborn", "fetch=shallow", "object-format=sha1"}
const uploadV0Caps = "symref=HEAD:refs/heads/main side-band-64k shallow thin-pack ofs-delta agent=git-forge/1.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta agent=git-forge/1.0"

var advV2 []byte

func init() {
	items := []interface{}{"version 2\n"}
	for _, c := range uploadV2Caps {
		items = append(items, c+"\n")
	}
	items = append(items, "flush")
	advV2 = pktLines(items)
}

func advertisementV2() []byte { return advV2 }

func advertisementV0(repo *Repo, service string) []byte {
	refs := sortedRefs(repo)
	var sb strings.Builder
	for _, r := range refs {
		sb.WriteString(r.name)
		sb.WriteByte('=')
		sb.WriteString(r.oid)
		sb.WriteByte(';')
	}
	key := service + "|" + sb.String()
	if cached, ok := repo.AdvCache[key]; ok {
		return cached
	}
	built := buildAdvertisementV0(repo, service, refs)
	if len(repo.AdvCache) > 16 {
		repo.AdvCache = make(map[string][]byte)
	}
	repo.AdvCache[key] = built
	return built
}

func buildAdvertisementV0(repo *Repo, service string, refs []refPair) []byte {
	built, _ := buildAdvertisementV0Offs(repo, service, refs)
	return built
}

// advOff records where one 40-char OID sits in an advertisement and what it
// stands for, so the ping-pong patch can refresh each one independently.
type advOff struct {
	offset int
	kind   string // "head" | "ref" | "peeled"
	ref    string // ref name for kind ref/peeled
}

func buildAdvertisementV0Offs(repo *Repo, service string, refs []refPair) ([]byte, []advOff) {
	caps := uploadV0Caps
	if service == "git-receive-pack" {
		caps = receiveV0Caps
	}
	var out []byte
	out = append(out, pktLineStr("# service="+service+"\n")...)
	out = append(out, pktFlushBytes...)
	tip, hasTip := headTarget(repo)
	var offs []advOff
	appendLine := func(line string, off *advOff) {
		if off != nil {
			off.offset = len(out) + 4
			offs = append(offs, *off)
		}
		out = append(out, pktLineStr(line)...)
	}
	if len(refs) == 0 || !hasTip {
		appendLine(zeroOID+" capabilities^{}\x00"+caps+"\n", nil)
	} else {
		appendLine(tip+" HEAD\x00"+caps+"\n", &advOff{kind: "head"})
		for _, r := range refs {
			appendLine(r.oid+" "+r.name+"\n", &advOff{kind: "ref", ref: r.name})
			if obj := repo.Objects[r.oid]; obj != nil && obj.Type == objTag {
				if peeled := peelToCommit(repo, r.oid); peeled != "" {
					appendLine(peeled+" "+r.name+"^{}\n", &advOff{kind: "peeled", ref: r.name})
				}
			}
		}
	}
	out = append(out, pktFlushBytes...)
	return out, offs
}

// receivePackAdv returns the receive-pack advertisement, patching the tip
// OID in place (ping-pong buffers) when only tips moved. Each recorded
// offset is patched with the OID it stands for (HEAD, its own ref, or the
// peeled commit of a tag).
func receivePackAdv(repo *Repo) []byte {
	refs := sortedRefs(repo)
	var fp strings.Builder
	for _, r := range refs {
		fp.WriteString(r.name)
		fp.WriteByte(';')
		// A tag's peeled line appears only when the tag peels; include the
		// tag target so a new tag object rebuilds the template.
		if obj := repo.Objects[r.oid]; obj != nil && obj.Type == objTag {
			fp.WriteString(peelToCommit(repo, r.oid))
			fp.WriteByte(';')
		}
	}
	fingerprint := fp.String()
	tip, hasTip := headTarget(repo)
	if !hasTip {
		tip = zeroOID
	}
	patchOID := func(o advOff) string {
		switch o.kind {
		case "head":
			return tip
		case "ref":
			if v, ok := repo.Refs[o.ref]; ok {
				return v
			}
			return zeroOID
		case "peeled":
			if v := peelToCommit(repo, repo.Refs[o.ref]); v != "" {
				return v
			}
			return zeroOID
		}
		return zeroOID
	}
	if repo.recvAdvRef == fingerprint && len(repo.recvAdvOffs) > 0 {
		other := 1 - repo.recvAdvFlip
		dst := make([]byte, len(repo.recvAdvBuf[repo.recvAdvFlip]))
		copy(dst, repo.recvAdvBuf[repo.recvAdvFlip])
		for _, off := range repo.recvAdvOffs {
			copy(dst[off.offset:off.offset+40], patchOID(off))
		}
		repo.recvAdvBuf[other] = dst
		repo.recvAdvFlip = other
		repo.recvAdvOID = tip
		return dst
	}
	built, offs := buildAdvertisementV0Offs(repo, "git-receive-pack", refs)
	slot := 1 - repo.recvAdvFlip
	repo.recvAdvBuf[slot] = built
	repo.recvAdvFlip = slot
	repo.recvAdvRef = fingerprint
	repo.recvAdvOffs = offs
	repo.recvAdvOID = tip
	return built
}

func hex4val(b []byte) int {
	v := 0
	for _, c := range b {
		v *= 16
		switch {
		case c >= '0' && c <= '9':
			v += int(c - '0')
		case c >= 'a' && c <= 'f':
			v += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v += int(c-'A') + 10
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// ls-refs (v2)
// ---------------------------------------------------------------------------

type LsRefsArgs struct {
	Peel     bool
	Symrefs  bool
	Unborn   bool
	Prefixes []string
}

func parseLsRefsArgs(packets []Packet) LsRefsArgs {
	args := LsRefsArgs{}
	seenDelim := false
	for _, p := range packets {
		if p.Kind == "delim" {
			seenDelim = true
			continue
		}
		if p.Kind != "data" {
			continue
		}
		line := strings.TrimSpace(string(p.Payload))
		if !seenDelim {
			continue
		}
		switch {
		case line == "peel":
			args.Peel = true
		case line == "symrefs":
			args.Symrefs = true
		case line == "unborn":
			args.Unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			args.Prefixes = append(args.Prefixes, line[len("ref-prefix "):])
		}
	}
	return args
}

func lsRefsResponse(repo *Repo, args LsRefsArgs) []byte {
	// Cache by args + ref fingerprint (clone workload calls this 50x).
	refs := sortedRefs(repo)
	var fp strings.Builder
	fp.WriteString(fmt.Sprintf("%v|%v|%v|%v|", args.Peel, args.Symrefs, args.Unborn, args.Prefixes))
	for _, r := range refs {
		fp.WriteString(r.name)
		fp.WriteByte('=')
		fp.WriteString(r.oid)
		fp.WriteByte(';')
	}
	key := fp.String()
	if cached, ok := repo.LsRefsCache[key]; ok {
		return cached
	}
	var lines []string
	tip, hasTip := headTarget(repo)
	keep := func(name string) bool {
		if len(args.Prefixes) == 0 {
			return true
		}
		for _, p := range args.Prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if len(refs) == 0 {
		if args.Unborn {
			lines = append(lines, "unborn HEAD symref-target:"+defaultBranchRef(repo)+"\n")
		}
	} else {
		if hasTip && keep("HEAD") && len(args.Prefixes) == 0 {
			if args.Symrefs {
				lines = append(lines, tip+" HEAD symref-target:"+defaultBranchRef(repo)+"\n")
			} else {
				lines = append(lines, tip+" HEAD\n")
			}
		}
		for _, r := range refs {
			if !keep(r.name) {
				continue
			}
			line := r.oid + " " + r.name
			if args.Peel {
				if obj := repo.Objects[r.oid]; obj != nil && obj.Type == objTag {
					if peeled := peelToCommit(repo, r.oid); peeled != "" {
						line += " peeled:" + peeled
					}
				}
			}
			lines = append(lines, line+"\n")
		}
	}
	items := make([]interface{}, 0, len(lines)+1)
	for _, l := range lines {
		items = append(items, l)
	}
	items = append(items, "flush")
	built := pktLines(items)
	if len(repo.LsRefsCache) > 64 {
		repo.LsRefsCache = make(map[string][]byte)
	}
	repo.LsRefsCache[key] = built
	return built
}

// ---------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------

type FetchArgs struct {
	Wants       []string
	Haves       []string
	Shallow     []string
	Depth       *int
	DeepenSince *int
	DeepenNot   []string
}

func parseFetchV2Args(packets []Packet) FetchArgs {
	args := FetchArgs{}
	seenDelim := false
	for _, p := range packets {
		if p.Kind == "delim" {
			seenDelim = true
			continue
		}
		if p.Kind != "data" {
			continue
		}
		line := strings.TrimSpace(string(p.Payload))
		if !seenDelim {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			args.Wants = append(args.Wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			args.Haves = append(args.Haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.Shallow = append(args.Shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if v, ok := parseJSInt(strings.TrimSpace(line[7:])); ok {
				args.Depth = &v
			}
		case strings.HasPrefix(line, "deepen-since "):
			if v, ok := parseJSInt(strings.TrimSpace(line[13:])); ok {
				args.DeepenSince = &v
			}
		case strings.HasPrefix(line, "deepen-not "):
			args.DeepenNot = append(args.DeepenNot, strings.TrimSpace(line[11:]))
		}
	}
	return args
}

func parseFetchV0Args(packets []Packet) FetchArgs {
	args := FetchArgs{}
	section := 0
	for _, p := range packets {
		if p.Kind == "flush" {
			section++
			continue
		}
		if p.Kind != "data" {
			continue
		}
		line := strings.TrimSpace(string(p.Payload))
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				rest := strings.TrimSpace(line[5:])
				if sp := strings.IndexByte(rest, ' '); sp >= 0 {
					rest = rest[:sp]
				}
				args.Wants = append(args.Wants, rest)
			case strings.HasPrefix(line, "shallow "):
				args.Shallow = append(args.Shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				if v, ok := parseJSInt(strings.TrimSpace(line[7:])); ok {
					args.Depth = &v
				}
			case strings.HasPrefix(line, "deepen-since "):
				if v, ok := parseJSInt(strings.TrimSpace(line[13:])); ok {
					args.DeepenSince = &v
				}
			case strings.HasPrefix(line, "deepen-not "):
				args.DeepenNot = append(args.DeepenNot, strings.TrimSpace(line[11:]))
			}
		} else {
			if strings.HasPrefix(line, "have ") {
				args.Haves = append(args.Haves, strings.TrimSpace(line[5:]))
			}
		}
	}
	return args
}

type FetchPlan struct {
	Objects   []*GitObject
	Shallow   []string
	Unshallow []string
}

func planFetch(repo *Repo, args FetchArgs) FetchPlan {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	haveReachable := collectHaveReachable(args.Haves, args.Shallow, lookup)
	included := make(map[string]struct{})
	var order []string
	add := func(oid string) {
		if _, ok := included[oid]; !ok {
			included[oid] = struct{}{}
			order = append(order, oid)
		}
	}
	var shallowSet map[string]struct{}
	if args.Depth == nil {
		for oid := range collectReachable(args.Wants, lookup) {
			add(oid)
		}
	} else {
		depth := *args.Depth
		shallowSet = make(map[string]struct{})
		type item struct {
			oid string
			gen int
		}
		var queue []item
		for _, w := range args.Wants {
			if isOID(w) {
				queue = append(queue, item{w, 1})
			}
		}
		seenCommits := make(map[string]struct{})
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if _, ok := seenCommits[cur.oid]; ok {
				continue
			}
			seenCommits[cur.oid] = struct{}{}
			obj := repo.Objects[cur.oid]
			if obj == nil {
				continue
			}
			if obj.Type == objTag {
				target := parseTag(obj.Data).Object
				add(cur.oid)
				if isOID(target) {
					queue = append(queue, item{target, cur.gen})
				}
				continue
			}
			if obj.Type != objCommit {
				add(cur.oid)
				continue
			}
			add(cur.oid)
			c := parseCommit(obj.Data)
			for extra := range collectReachable([]string{c.Tree}, lookup) {
				add(extra)
			}
			if cur.gen >= depth {
				shallowSet[cur.oid] = struct{}{}
			} else {
				for _, p := range c.Parents {
					queue = append(queue, item{p, cur.gen + 1})
				}
			}
		}
	}
	for oid := range haveReachable {
		if _, ok := included[oid]; ok {
			delete(included, oid)
		}
	}
	// Rebuild order without removed oids.
	var keptOrder []string
	for _, oid := range order {
		if _, ok := included[oid]; ok {
			keptOrder = append(keptOrder, oid)
		}
	}
	var shallow []string
	if shallowSet != nil {
		for oid := range shallowSet {
			shallow = append(shallow, oid)
		}
		sort.Strings(shallow)
	}
	var unshallow []string
	for _, oid := range args.Shallow {
		if _, isShallow := shallowSet[oid]; !isShallow {
			if _, ok := included[oid]; ok {
				unshallow = append(unshallow, oid)
			}
		}
	}
	sort.Strings(unshallow)
	var objects []*GitObject
	for _, oid := range keptOrder {
		if obj := repo.Objects[oid]; obj != nil {
			objects = append(objects, obj)
		}
	}
	typeRank := func(t gitObjectType) int {
		switch t {
		case objCommit:
			return 0
		case objTree:
			return 1
		case objTag:
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(objects, func(i, j int) bool {
		return typeRank(objects[i].Type) < typeRank(objects[j].Type)
	})
	return FetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow}
}

func fetchCacheKey(repo *Repo, protocol string, args FetchArgs) string {
	tip := ""
	if t, ok := headTarget(repo); ok {
		tip = t
	}
	depth := ""
	if args.Depth != nil {
		depth = strconvItoa(*args.Depth)
	}
	since := ""
	if args.DeepenSince != nil {
		since = strconvItoa(*args.DeepenSince)
	}
	return strings.Join([]string{
		protocol, strconvItoa(repo.ID), tip, strconvItoa(len(repo.Objects)),
		strings.Join(args.Wants, ","), strings.Join(args.Haves, ","),
		strings.Join(args.Shallow, ","), depth, since,
		strings.Join(args.DeepenNot, ","),
	}, "|")
}

func cacheFetchResponse(repo *Repo, key string, body []byte) {
	if len(body) > 2*1024*1024 {
		return
	}
	if len(repo.FetchCache) > 64 {
		repo.FetchCache = make(map[string][]byte)
	}
	repo.FetchCache[key] = body
}

func fetchV2Response(repo *Repo, args FetchArgs) []byte {
	// Clone fast path: depth-1 fetch of the current tip, no haves.
	if args.Depth != nil && *args.Depth == 1 && len(args.Wants) == 1 &&
		len(args.Haves) == 0 && len(args.Shallow) == 0 && args.DeepenSince == nil && len(args.DeepenNot) == 0 {
		if tip, ok := headTarget(repo); ok && tip == args.Wants[0] {
			if cached := repo.cloneResp.Load(); cached != nil && repo.cloneTip == tip {
				return *cached
			}
		}
	}
	key := fetchCacheKey(repo, "v2", args)
	if cached, ok := repo.FetchCache[key]; ok {
		return cached
	}
	built := buildFetchV2Response(repo, args)
	cacheFetchResponse(repo, key, built)
	if args.Depth != nil && *args.Depth == 1 && len(args.Wants) == 1 &&
		len(args.Haves) == 0 && len(args.Shallow) == 0 && args.DeepenSince == nil && len(args.DeepenNot) == 0 {
		if tip, ok := headTarget(repo); ok && tip == args.Wants[0] {
			cp := built
			repo.cloneTip = tip
			repo.cloneResp.Store(&cp)
		}
	}
	return built
}

func buildFetchV2Response(repo *Repo, args FetchArgs) []byte {
	plan := planFetch(repo, args)
	var out []byte
	out = append(out, pktLines([]interface{}{"acknowledgments\n", "NAK\n", "delim"})...)
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		var lines []interface{}
		lines = append(lines, "shallow-info\n")
		for _, oid := range plan.Shallow {
			lines = append(lines, "shallow "+oid+"\n")
		}
		for _, oid := range plan.Unshallow {
			lines = append(lines, "unshallow "+oid+"\n")
		}
		lines = append(lines, "delim")
		out = append(out, pktLines(lines)...)
	}
	if len(plan.Objects) > 0 {
		pack := writePack(plan.Objects)
		out = append(out, pktLineStr("packfile\n")...)
		out = append(out, sidebandWrap(pack, 32768)...)
	}
	out = append(out, pktFlushBytes...)
	return out
}

func fetchV0Response(repo *Repo, args FetchArgs) []byte {
	key := fetchCacheKey(repo, "v0", args)
	if cached, ok := repo.FetchCache[key]; ok {
		return cached
	}
	built := buildFetchV0Response(repo, args)
	cacheFetchResponse(repo, key, built)
	return built
}

func buildFetchV0Response(repo *Repo, args FetchArgs) []byte {
	plan := planFetch(repo, args)
	var lines []interface{}
	lines = append(lines, "NAK\n")
	for _, oid := range plan.Shallow {
		lines = append(lines, "shallow "+oid+"\n")
	}
	for _, oid := range plan.Unshallow {
		lines = append(lines, "unshallow "+oid+"\n")
	}
	lines = append(lines, "flush")
	var out []byte
	out = append(out, pktLines(lines)...)
	if len(plan.Objects) > 0 {
		out = append(out, sidebandWrap(writePack(plan.Objects), 32768)...)
	}
	out = append(out, pktFlushBytes...)
	return out
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

type PushReportLine struct {
	Ref    string
	OK     bool
	Reason string
}

func parseReceivePack(body []byte) (commands []PushCommand, pack []byte, caps []string, err error) {
	offset := 0
	sawFlush := false
	for offset < len(body) {
		if offset+4 > len(body) {
			return nil, nil, nil, errors.New("truncated pkt-line in push")
		}
		header := body[offset : offset+4]
		if !isHex4(header) {
			break
		}
		length := hex4val(header)
		if length == 0 {
			offset += 4
			sawFlush = true
			break
		}
		if length == 1 || length == 2 {
			offset += 4
			continue
		}
		if offset+length > len(body) {
			return nil, nil, nil, errors.New("truncated pkt-line in push")
		}
		line := string(body[offset+4 : offset+length])
		offset += length
		text := line
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = strings.Fields(strings.TrimSpace(line[nul+1:]))
			text = line[:nul]
		}
		parts := strings.Split(strings.TrimSpace(text), " ")
		if len(parts) >= 3 {
			commands = append(commands, PushCommand{
				OldOID: parts[0],
				NewOID: parts[1],
				Ref:    strings.Join(parts[2:], " "),
			})
		}
	}
	if !sawFlush && len(commands) == 0 {
		return nil, nil, nil, errors.New("push has no command list")
	}
	return commands, body[offset:], caps, nil
}

func applyPush(repo *Repo, store *Store, commands []PushCommand, packObjects map[string]*GitObject) []PushReportLine {
	for oid, obj := range packObjects {
		if _, ok := repo.Objects[oid]; !ok {
			repo.Objects[oid] = obj
		}
	}
	var report []PushReportLine
	for _, cmd := range commands {
		current, exists := repo.Refs[cmd.Ref]
		if cmd.NewOID == zeroOID {
			if !exists {
				report = append(report, PushReportLine{Ref: cmd.Ref, Reason: "non-fast-forward"})
			} else if cmd.OldOID != zeroOID && cmd.OldOID != current {
				report = append(report, PushReportLine{Ref: cmd.Ref, Reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, cmd.Ref)
				report = append(report, PushReportLine{Ref: cmd.Ref, OK: true})
			}
			continue
		}
		if _, ok := repo.Objects[cmd.NewOID]; !ok {
			report = append(report, PushReportLine{Ref: cmd.Ref, Reason: "non-fast-forward"})
			continue
		}
		if !exists {
			if cmd.OldOID != zeroOID {
				report = append(report, PushReportLine{Ref: cmd.Ref, Reason: "non-fast-forward"})
				continue
			}
			repo.Refs[cmd.Ref] = cmd.NewOID
			report = append(report, PushReportLine{Ref: cmd.Ref, OK: true})
			continue
		}
		if cmd.OldOID != current {
			report = append(report, PushReportLine{Ref: cmd.Ref, Reason: "non-fast-forward"})
			continue
		}
		if cmd.NewOID == current || isAncestor(repo, current, cmd.NewOID) {
			repo.Refs[cmd.Ref] = cmd.NewOID
			report = append(report, PushReportLine{Ref: cmd.Ref, OK: true})
		} else {
			report = append(report, PushReportLine{Ref: cmd.Ref, Reason: "non-fast-forward"})
		}
	}
	// Any ref move invalidates the git caches.
	repo.FetchCache = make(map[string][]byte)
	repo.AdvCache = make(map[string][]byte)
	repo.LsRefsCache = make(map[string][]byte)
	repo.cloneResp.Store(nil)
	repo.cloneTip = ""
	return report
}

func pushReportResponse(report []PushReportLine, sideband bool) []byte {
	var lines []interface{}
	lines = append(lines, "unpack ok\n")
	for _, e := range report {
		if e.OK {
			lines = append(lines, "ok "+e.Ref+"\n")
		} else {
			lines = append(lines, "ng "+e.Ref+" "+e.Reason+"\n")
		}
	}
	lines = append(lines, "flush")
	stream := pktLines(lines)
	if !sideband {
		return append(append([]byte{}, stream...), pktFlushBytes...)
	}
	out := sidebandWrap(stream, 32768)
	return append(out, pktFlushBytes...)
}

func makeServerCommit(store *Store, repo *Repo, tree string, parents []string, message string) string {
	timestamp := store.tickLocked()
	data := buildCommitText(tree, parents, message, timestamp)
	return putObject(repo, objCommit, data)
}
