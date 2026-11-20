package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	ZeroOID          = "0000000000000000000000000000000000000000"
	PktLineMax       = 65520
	SidebandChunkMax = 65515
)

var (
	ErrMalformedPktLine = errors.New("malformed pkt-line")
	ErrPackFormat       = errors.New("invalid packfile format")

	FlushPkt       = []byte("0000")
	DelimPkt       = []byte("0001")
	ResponseEndPkt = []byte("0002")

	EmptyPackfile = BuildPackfile(nil)
)

type GitObject struct {
	OID      string
	Type     string
	Data     []byte
	Deflated []byte
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

type PathBlobEntry struct {
	Mode string
	OID  string
}

type FileDiffItem struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

type PktPacket struct {
	Kind    byte // 'F'=flush, 'D'=delim, 'R'=response-end, 'P'=data
	Payload []byte
}

func IsValidOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

var sha1Pool = sync.Pool{
	New: func() any {
		return sha1.New()
	},
}

func HashGitObject(objType string, data []byte) string {
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	var hdrBuf [40]byte
	hdr := hdrBuf[:0]
	hdr = append(hdr, objType...)
	hdr = append(hdr, ' ')
	hdr = strconv.AppendInt(hdr, int64(len(data)), 10)
	hdr = append(hdr, 0)
	h.Write(hdr)
	h.Write(data)
	var sum [20]byte
	h.Sum(sum[:0])
	sha1Pool.Put(h)
	return hex.EncodeToString(sum[:])
}

func ExtractCommitParents(data []byte) []string {
	var parents []string
	off := 0
	for off < len(data) {
		nl := bytes.IndexByte(data[off:], '\n')
		var line []byte
		if nl < 0 {
			line = data[off:]
			off = len(data)
		} else {
			line = data[off : off+nl]
			off += nl + 1
		}
		if len(line) == 0 {
			break
		}
		if len(line) == 47 && bytes.HasPrefix(line, []byte("parent ")) {
			parents = append(parents, string(line[7:]))
		}
	}
	return parents
}

func CreateGitObject(objType string, data []byte, deflated []byte) *GitObject {
	return &GitObject{
		OID:      HashGitObject(objType, data),
		Type:     objType,
		Data:     data,
		Deflated: deflated,
	}
}

func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			break
		}
		mode := string(data[offset : offset+space])
		nameStart := offset + space + 1
		nul := bytes.IndexByte(data[nameStart:], 0)
		if nul < 0 || nameStart+nul+21 > len(data) {
			break
		}
		name := string(data[nameStart : nameStart+nul])
		oidStart := nameStart + nul + 1
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = oidStart + 20
	}
	return entries
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	total := 0
	for _, e := range sorted {
		total += len(e.Mode) + 1 + len(e.Name) + 1 + 20
	}
	out := make([]byte, 0, total)
	var binOID [20]byte
	for _, e := range sorted {
		out = append(out, e.Mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		hex.Decode(binOID[:], []byte(e.OID))
		out = append(out, binOID[:]...)
	}
	return out
}

func ParseCommit(data []byte) CommitView {
	sep := bytes.Index(data, []byte("\n\n"))
	hdr := data
	msg := ""
	if sep >= 0 {
		hdr = data[:sep]
		msg = string(data[sep+2:])
	}
	var view CommitView
	view.Message = msg
	for _, line := range strings.Split(string(hdr), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k := line[:sp]
		v := line[sp+1:]
		switch k {
		case "tree":
			view.Tree = v
		case "parent":
			view.Parents = append(view.Parents, v)
		case "author":
			view.Author = v
		case "committer":
			view.Committer = v
		}
	}
	return view
}

func BuildCommit(tree string, parents []string, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(tree)
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(author)
	b.WriteByte('\n')
	b.WriteString("committer ")
	b.WriteString(committer)
	b.WriteString("\n\n")
	b.WriteString(message)
	return []byte(b.String())
}

func ParseTag(data []byte) TagView {
	sep := bytes.Index(data, []byte("\n\n"))
	hdr := data
	msg := ""
	if sep >= 0 {
		hdr = data[:sep]
		msg = string(data[sep+2:])
	}
	view := TagView{Type: "commit", Message: msg}
	for _, line := range strings.Split(string(hdr), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k := line[:sp]
		v := line[sp+1:]
		switch k {
		case "object":
			view.Object = v
		case "type":
			view.Type = v
		case "tag":
			view.Tag = v
		case "tagger":
			view.Tagger = v
		}
	}
	return view
}

func PeelOID(objects map[string]*GitObject, startOID string) string {
	cur := startOID
	for i := 0; i < 16; i++ {
		obj := objects[cur]
		if obj == nil || obj.Type != "tag" {
			return cur
		}
		cur = ParseTag(obj.Data).Object
	}
	return cur
}

func ResolveTreeOID(objects map[string]*GitObject, oid string) string {
	peeled := PeelOID(objects, oid)
	obj := objects[peeled]
	if obj == nil {
		return ""
	}
	if obj.Type == "tree" {
		return obj.OID
	}
	if obj.Type == "commit" {
		return ParseCommit(obj.Data).Tree
	}
	return ""
}

func FlattenTree(objects map[string]*GitObject, commitOrTreeOID string) map[string]PathBlobEntry {
	out := make(map[string]PathBlobEntry)
	rootTree := ResolveTreeOID(objects, commitOrTreeOID)
	if rootTree == "" {
		return out
	}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		treeObj := objects[treeOID]
		if treeObj == nil || treeObj.Type != "tree" {
			return
		}
		for _, entry := range ParseTree(treeObj.Data) {
			fullPath := entry.Name
			if prefix != "" {
				fullPath = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, fullPath)
			} else if entry.Mode != "160000" {
				out[fullPath] = PathBlobEntry{Mode: entry.Mode, OID: entry.OID}
			}
		}
	}
	walk(rootTree, "")
	return out
}

type treeDirNode struct {
	files map[string]PathBlobEntry
	dirs  map[string]*treeDirNode
}

func newTreeDirNode() *treeDirNode {
	return &treeDirNode{
		files: make(map[string]PathBlobEntry),
		dirs:  make(map[string]*treeDirNode),
	}
}

func BuildTreeFromPaths(saveObj func(*GitObject) *GitObject, pathMap map[string]PathBlobEntry) string {
	root := newTreeDirNode()
	for rawPath, entry := range pathMap {
		parts := strings.Split(rawPath, "/")
		var clean []string
		for _, p := range parts {
			if p != "" {
				clean = append(clean, p)
			}
		}
		if len(clean) == 0 {
			continue
		}
		node := root
		for i := 0; i < len(clean)-1; i++ {
			seg := clean[i]
			child := node.dirs[seg]
			if child == nil {
				child = newTreeDirNode()
				node.dirs[seg] = child
			}
			node = child
		}
		node.files[clean[len(clean)-1]] = entry
	}

	var writeNode func(n *treeDirNode) string
	writeNode = func(n *treeDirNode) string {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, file := range n.files {
			entries = append(entries, TreeEntry{Mode: file.Mode, Name: name, OID: file.OID})
		}
		for name, child := range n.dirs {
			childOID := writeNode(child)
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: childOID})
		}
		saved := saveObj(CreateGitObject("tree", BuildTree(entries), nil))
		return saved.OID
	}

	return writeNode(root)
}

func IsAncestor(commitParents map[string][]string, ancestorOID, descendantOID string) bool {
	if ancestorOID == descendantOID {
		return true
	}
	direct := commitParents[descendantOID]
	for _, p := range direct {
		if p == ancestorOID {
			return true
		}
	}
	visited := map[string]struct{}{descendantOID: {}}
	queue := []string{descendantOID}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, p := range commitParents[cur] {
			if p == ancestorOID {
				return true
			}
			if _, ok := visited[p]; !ok {
				visited[p] = struct{}{}
				queue = append(queue, p)
			}
		}
	}
	return false
}

func FindMergeBase(commitParents map[string][]string, oidA, oidB string) string {
	if oidA == oidB {
		return oidA
	}
	ancA := map[string]struct{}{oidA: {}}
	qA := []string{oidA}
	for i := 0; i < len(qA); i++ {
		cur := qA[i]
		for _, p := range commitParents[cur] {
			if _, ok := ancA[p]; !ok {
				ancA[p] = struct{}{}
				qA = append(qA, p)
			}
		}
	}
	if _, ok := ancA[oidB]; ok {
		return oidB
	}
	visitedB := map[string]struct{}{oidB: {}}
	qB := []string{oidB}
	for i := 0; i < len(qB); i++ {
		cur := qB[i]
		if _, ok := ancA[cur]; ok {
			return cur
		}
		for _, p := range commitParents[cur] {
			if _, ok := visitedB[p]; !ok {
				visitedB[p] = struct{}{}
				qB = append(qB, p)
			}
		}
	}
	return ""
}

func samePathEntry(a, b PathBlobEntry, aOK, bOK bool) bool {
	if !aOK && !bOK {
		return true
	}
	if aOK != bOK {
		return false
	}
	return a.OID == b.OID && a.Mode == b.Mode
}

func ThreeWayMergeTrees(
	saveObj func(*GitObject) *GitObject,
	ancMap, baseMap, headMap map[string]PathBlobEntry,
) (string, bool) {
	allPaths := make(map[string]struct{}, len(ancMap)+len(baseMap)+len(headMap))
	for k := range ancMap {
		allPaths[k] = struct{}{}
	}
	for k := range baseMap {
		allPaths[k] = struct{}{}
	}
	for k := range headMap {
		allPaths[k] = struct{}{}
	}
	merged := make(map[string]PathBlobEntry)
	for path := range allPaths {
		anc, ancOK := ancMap[path]
		base, baseOK := baseMap[path]
		head, headOK := headMap[path]
		if samePathEntry(base, head, baseOK, headOK) {
			if baseOK {
				merged[path] = base
			}
		} else if samePathEntry(base, anc, baseOK, ancOK) {
			if headOK {
				merged[path] = head
			}
		} else if samePathEntry(head, anc, headOK, ancOK) {
			if baseOK {
				merged[path] = base
			}
		} else {
			return "", false
		}
	}
	return BuildTreeFromPaths(saveObj, merged), true
}

func splitLinesNoTrail(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	raw := strings.Split(string(data), "\n")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	return raw
}

func countLineDelta(baseData, headData []byte) (int, int) {
	baseLines := splitLinesNoTrail(baseData)
	headLines := splitLinesNoTrail(headData)
	start := 0
	minLen := len(baseLines)
	if len(headLines) < minLen {
		minLen = len(headLines)
	}
	for start < minLen && baseLines[start] == headLines[start] {
		start++
	}
	baseEnd := len(baseLines)
	headEnd := len(headLines)
	for baseEnd > start && headEnd > start && baseLines[baseEnd-1] == headLines[headEnd-1] {
		baseEnd--
		headEnd--
	}
	deletions := baseEnd - start
	additions := headEnd - start
	if additions == 0 && deletions == 0 && len(baseData) != len(headData) {
		return 1, 1
	}
	return additions, deletions
}

func DiffTrees(objects map[string]*GitObject, baseMap, headMap map[string]PathBlobEntry) []FileDiffItem {
	allPathsSet := make(map[string]struct{}, len(baseMap)+len(headMap))
	for k := range baseMap {
		allPathsSet[k] = struct{}{}
	}
	for k := range headMap {
		allPathsSet[k] = struct{}{}
	}
	allPaths := make([]string, 0, len(allPathsSet))
	for k := range allPathsSet {
		allPaths = append(allPaths, k)
	}
	sort.Strings(allPaths)

	diffs := make([]FileDiffItem, 0)
	for _, fn := range allPaths {
		b, bOK := baseMap[fn]
		h, hOK := headMap[fn]
		if samePathEntry(b, h, bOK, hOK) {
			continue
		}
		if !bOK && hOK {
			var hData []byte
			if obj := objects[h.OID]; obj != nil {
				hData = obj.Data
			}
			adds := len(splitLinesNoTrail(hData))
			if adds < 1 {
				adds = 1
			}
			diffs = append(diffs, FileDiffItem{Filename: fn, Status: "added", Additions: adds, Deletions: 0, Changes: adds})
		} else if bOK && !hOK {
			var bData []byte
			if obj := objects[b.OID]; obj != nil {
				bData = obj.Data
			}
			dels := len(splitLinesNoTrail(bData))
			if dels < 1 {
				dels = 1
			}
			diffs = append(diffs, FileDiffItem{Filename: fn, Status: "removed", Additions: 0, Deletions: dels, Changes: dels})
		} else {
			var bData, hData []byte
			if obj := objects[b.OID]; obj != nil {
				bData = obj.Data
			}
			if obj := objects[h.OID]; obj != nil {
				hData = obj.Data
			}
			adds, dels := countLineDelta(bData, hData)
			if adds == 0 && dels == 0 {
				adds = 1
			}
			diffs = append(diffs, FileDiffItem{Filename: fn, Status: "modified", Additions: adds, Deletions: dels, Changes: adds + dels})
		}
	}
	return diffs
}

const hexDigits = "0123456789abcdef"

func AppendPktLine(dst []byte, payload []byte) []byte {
	total := len(payload) + 4
	dst = append(dst, hexDigits[(total>>12)&0xf], hexDigits[(total>>8)&0xf], hexDigits[(total>>4)&0xf], hexDigits[total&0xf])
	return append(dst, payload...)
}

func AppendPktLineStr(dst []byte, s string) []byte {
	total := len(s) + 4
	dst = append(dst, hexDigits[(total>>12)&0xf], hexDigits[(total>>8)&0xf], hexDigits[(total>>4)&0xf], hexDigits[total&0xf])
	return append(dst, s...)
}

func AppendSideband(dst []byte, band byte, data []byte) []byte {
	for offset := 0; offset < len(data); {
		end := offset + SidebandChunkMax
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		total := 5 + len(chunk)
		dst = append(dst, hexDigits[(total>>12)&0xf], hexDigits[(total>>8)&0xf], hexDigits[(total>>4)&0xf], hexDigits[total&0xf], band)
		dst = append(dst, chunk...)
		offset = end
	}
	return dst
}

func parseHex4(buf []byte, off int) (int, error) {
	val := 0
	for i := 0; i < 4; i++ {
		c := buf[off+i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			d = int(c - 'A' + 10)
		default:
			return 0, ErrMalformedPktLine
		}
		val = (val << 4) | d
	}
	return val, nil
}

func DecodePktLines(buf []byte, stopAtFlush bool) ([]PktPacket, int, error) {
	packets := make([]PktPacket, 0, 16)
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, 0, ErrMalformedPktLine
		}
		l, err := parseHex4(buf, off)
		if err != nil {
			return nil, 0, err
		}
		if l == 0 {
			packets = append(packets, PktPacket{Kind: 'F'})
			off += 4
			if stopAtFlush {
				break
			}
			continue
		}
		if l == 1 {
			packets = append(packets, PktPacket{Kind: 'D'})
			off += 4
			continue
		}
		if l == 2 {
			packets = append(packets, PktPacket{Kind: 'R'})
			off += 4
			continue
		}
		if l < 4 || l > PktLineMax || off+l > len(buf) {
			return nil, 0, ErrMalformedPktLine
		}
		packets = append(packets, PktPacket{Kind: 'P', Payload: buf[off+4 : off+l]})
		off += l
	}
	return packets, off, nil
}

type sliceByteReader struct {
	data []byte
	pos  int
}

func (r *sliceByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *sliceByteReader) ReadByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

var zlibReaderPool sync.Pool

func inflateExact(src []byte, expectedSize int) ([]byte, int, error) {
	sr := sliceByteReader{data: src, pos: 0}
	var zr io.ReadCloser
	var err error
	if pooled := zlibReaderPool.Get(); pooled != nil {
		if resetter, ok := pooled.(zlib.Resetter); ok {
			if err = resetter.Reset(&sr, nil); err == nil {
				zr = pooled.(io.ReadCloser)
			}
		}
	}
	if zr == nil {
		sr.pos = 0
		zr, err = zlib.NewReader(&sr)
		if err != nil {
			return nil, 0, err
		}
	}

	out := make([]byte, expectedSize)
	total := 0
	for total < expectedSize {
		n, rErr := zr.Read(out[total:])
		total += n
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			zr.Close()
			zlibReaderPool.Put(zr)
			return nil, 0, rErr
		}
	}
	var extra [1]byte
	nExtra, rErr := zr.Read(extra[:])
	if nExtra > 0 || (rErr != nil && rErr != io.EOF) {
		zr.Close()
		zlibReaderPool.Put(zr)
		return nil, 0, fmt.Errorf("%w: inflated size exceeds %d", ErrPackFormat, expectedSize)
	}
	if total != expectedSize {
		zr.Close()
		zlibReaderPool.Put(zr)
		return nil, 0, fmt.Errorf("%w: inflated %d != %d", ErrPackFormat, total, expectedSize)
	}
	_ = zr.Close()
	zlibReaderPool.Put(zr)
	return out, sr.pos, nil
}

func readPackEntryHeader(buf []byte, start int) (int, int, int, error) {
	if start >= len(buf) {
		return 0, 0, 0, ErrPackFormat
	}
	b := buf[start]
	typeCode := int((b >> 4) & 0x07)
	size := int(b & 0x0f)
	shift := uint(4)
	off := start + 1
	for (b & 0x80) != 0 {
		if off >= len(buf) {
			return 0, 0, 0, ErrPackFormat
		}
		b = buf[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 56 {
			return 0, 0, 0, ErrPackFormat
		}
	}
	return typeCode, size, off, nil
}

func readOfsDistance(buf []byte, start int) (int, int, error) {
	if start >= len(buf) {
		return 0, 0, ErrPackFormat
	}
	b := buf[start]
	off := start + 1
	val := int(b & 0x7f)
	for (b & 0x80) != 0 {
		if off >= len(buf) {
			return 0, 0, ErrPackFormat
		}
		b = buf[off]
		off++
		val = (val+1)*128 + int(b&0x7f)
	}
	return val, off, nil
}

func readDeltaVarint(buf []byte, off *int) (int, error) {
	val := 0
	shift := uint(0)
	for {
		if *off >= len(buf) {
			return 0, ErrPackFormat
		}
		b := buf[*off]
		*off++
		val += int(b&0x7f) << shift
		if (b & 0x80) == 0 {
			break
		}
		shift += 7
		if shift > 56 {
			return 0, ErrPackFormat
		}
	}
	return val, nil
}

func ApplyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	srcSize, err := readDeltaVarint(delta, &off)
	if err != nil {
		return nil, err
	}
	dstSize, err := readDeltaVarint(delta, &off)
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, ErrPackFormat
	}
	out := make([]byte, dstSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if (op & 0x80) != 0 {
			copyOff := 0
			copySize := 0
			for bit := uint(0); bit < 4; bit++ {
				if (op & (1 << bit)) != 0 {
					if off >= len(delta) {
						return nil, ErrPackFormat
					}
					copyOff |= int(delta[off]) << (bit * 8)
					off++
				}
			}
			for bit := uint(0); bit < 3; bit++ {
				if (op & (1 << (bit + 4))) != 0 {
					if off >= len(delta) {
						return nil, ErrPackFormat
					}
					copySize |= int(delta[off]) << (bit * 8)
					off++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || written+copySize > dstSize {
				return nil, ErrPackFormat
			}
			copy(out[written:written+copySize], base[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			insLen := int(op & 0x7f)
			if off+insLen > len(delta) || written+insLen > dstSize {
				return nil, ErrPackFormat
			}
			copy(out[written:written+insLen], delta[off:off+insLen])
			off += insLen
			written += insLen
		} else {
			return nil, ErrPackFormat
		}
	}
	if written != dstSize {
		return nil, ErrPackFormat
	}
	return out, nil
}

func codeToTypeName(code int) string {
	switch code {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	default:
		return ""
	}
}

func typeNameToCode(t string) byte {
	switch t {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	default:
		return 0
	}
}

func UnpackPackfile(packBuf []byte, lookup func(string) *GitObject) ([]*GitObject, error) {
	if len(packBuf) < 32 {
		return nil, ErrPackFormat
	}
	if packBuf[0] != 'P' || packBuf[1] != 'A' || packBuf[2] != 'C' || packBuf[3] != 'K' {
		return nil, ErrPackFormat
	}
	ver := binary.BigEndian.Uint32(packBuf[4:8])
	if ver != 2 && ver != 3 {
		return nil, ErrPackFormat
	}
	count := int(binary.BigEndian.Uint32(packBuf[8:12]))
	trailerStart := len(packBuf) - 20
	expectedSum := sha1.Sum(packBuf[:trailerStart])
	if !bytes.Equal(expectedSum[:], packBuf[trailerStart:]) {
		return nil, ErrPackFormat
	}

	type parsedEntry struct {
		offset     int
		typeCode   int
		data       []byte
		deflated   []byte
		baseOffset int
		baseOID    string
		resolved   *GitObject
	}

	entries := make([]*parsedEntry, 0, count)
	byOffset := make(map[int]*parsedEntry, count)
	off := 12

	for i := 0; i < count; i++ {
		entryOff := off
		tc, sz, nextOff, err := readPackEntryHeader(packBuf, off)
		if err != nil {
			return nil, err
		}
		off = nextOff
		entry := &parsedEntry{offset: entryOff, typeCode: tc}
		if tc == 6 {
			dist, n2, err := readOfsDistance(packBuf, off)
			if err != nil {
				return nil, err
			}
			off = n2
			baseOff := entryOff - dist
			if baseOff < 12 || byOffset[baseOff] == nil {
				return nil, ErrPackFormat
			}
			entry.baseOffset = baseOff
		} else if tc == 7 {
			if off+20 > trailerStart {
				return nil, ErrPackFormat
			}
			entry.baseOID = hex.EncodeToString(packBuf[off : off+20])
			off += 20
		} else if codeToTypeName(tc) == "" {
			return nil, ErrPackFormat
		}

		zStart := off
		inflated, consumed, err := inflateExact(packBuf[off:trailerStart], sz)
		if err != nil {
			return nil, err
		}
		entry.data = inflated
		if tc != 6 && tc != 7 {
			defCopy := make([]byte, consumed)
			copy(defCopy, packBuf[zStart:zStart+consumed])
			entry.deflated = defCopy
		}
		off += consumed
		entries = append(entries, entry)
		byOffset[entryOff] = entry
	}

	if off != trailerStart {
		return nil, ErrPackFormat
	}

	byOID := make(map[string]*GitObject, len(entries))
	resolving := make(map[int]bool)

	var resolve func(e *parsedEntry) (*GitObject, error)
	resolve = func(e *parsedEntry) (*GitObject, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		if resolving[e.offset] {
			return nil, ErrPackFormat
		}
		resolving[e.offset] = true
		var obj *GitObject
		if e.typeCode == 6 {
			base, err := resolve(byOffset[e.baseOffset])
			if err != nil {
				return nil, err
			}
			applied, err := ApplyDelta(base.Data, e.data)
			if err != nil {
				return nil, err
			}
			obj = CreateGitObject(base.Type, applied, nil)
		} else if e.typeCode == 7 {
			base := byOID[e.baseOID]
			if base == nil && lookup != nil {
				base = lookup(e.baseOID)
			}
			if base == nil {
				return nil, ErrPackFormat
			}
			applied, err := ApplyDelta(base.Data, e.data)
			if err != nil {
				return nil, err
			}
			obj = CreateGitObject(base.Type, applied, nil)
		} else {
			obj = CreateGitObject(codeToTypeName(e.typeCode), e.data, e.deflated)
		}
		delete(resolving, e.offset)
		e.resolved = obj
		byOID[obj.OID] = obj
		return obj, nil
	}

	out := make([]*GitObject, len(entries))
	for i, e := range entries {
		obj, err := resolve(e)
		if err != nil {
			return nil, err
		}
		out[i] = obj
	}
	return out, nil
}

func appendPackObjectHeader(dst []byte, typeCode byte, size int) []byte {
	first := (typeCode << 4) | byte(size&0x0f)
	rem := size >> 4
	if rem > 0 {
		first |= 0x80
	}
	dst = append(dst, first)
	for rem > 0 {
		b := byte(rem & 0x7f)
		rem >>= 7
		if rem > 0 {
			b |= 0x80
		}
		dst = append(dst, b)
	}
	return dst
}

func deflateFast(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 16)
	zw, _ := zlib.NewWriterLevel(&buf, flate.BestSpeed)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

func BuildPackfile(objs []*GitObject) []byte {
	est := 32 + len(objs)*64
	out := make([]byte, 12, est)
	out[0], out[1], out[2], out[3] = 'P', 'A', 'C', 'K'
	binary.BigEndian.PutUint32(out[4:8], 2)
	binary.BigEndian.PutUint32(out[8:12], uint32(len(objs)))

	for _, obj := range objs {
		out = appendPackObjectHeader(out, typeNameToCode(obj.Type), len(obj.Data))
		if obj.Deflated == nil {
			obj.Deflated = deflateFast(obj.Data)
		}
		out = append(out, obj.Deflated...)
	}
	sum := sha1.Sum(out)
	out = append(out, sum[:]...)
	return out
}
