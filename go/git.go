// Git smart HTTP: pkt-line framing, pack v2 read/write, upload-pack
// (v2 + v0), receive-pack (v0). No git binary, stdlib only.
// Ported from the Node git.ts; behavior is identical.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- pkt-line

func pktLine(payload []byte) []byte {
	head := []byte(strconv.FormatInt(int64(len(payload)+4), 16))
	for len(head) < 4 {
		head = append([]byte("0"), head...)
	}
	out := make([]byte, 0, len(payload)+4)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pktKind int

const (
	pktLineKind pktKind = iota
	pktFlushKind
	pktDelimKind
	pktEndKind
)

type pktItem struct {
	kind pktKind
	data []byte
}

func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// parsePktLines parses pkt-lines from the front of buf; stops at end of buffer.
func parsePktLines(buf []byte) (items []pktItem, rest []byte, rawMode bool) {
	i := 0
	for i+4 <= len(buf) {
		head := buf[i : i+4]
		if !isHex4(head) {
			return items, buf[i:], true
		}
		length, _ := strconv.ParseInt(string(head), 16, 32)
		if length == 0 {
			items = append(items, pktItem{kind: pktFlushKind})
			i += 4
			continue
		}
		if length == 1 {
			items = append(items, pktItem{kind: pktDelimKind})
			i += 4
			continue
		}
		if length == 2 {
			items = append(items, pktItem{kind: pktEndKind})
			i += 4
			continue
		}
		if i+int(length) > len(buf) {
			break
		}
		data := make([]byte, int(length)-4)
		copy(data, buf[i+4:i+int(length)])
		items = append(items, pktItem{kind: pktLineKind, data: data})
		i += int(length)
	}
	return items, buf[i:], false
}

// ---------------------------------------------------------------- pack

var packTypes = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}
var typeCodes = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}

// inflateAt inflates one zlib stream starting at offset. compress/zlib over
// a bytes.Reader (an io.ByteReader, so flate reads it directly with no
// buffering) reports exactly how many compressed bytes the stream consumed
// via the reader's remaining length — one inflate per object, zero throws
// on the happy path.
func inflateAt(buf []byte, offset int) (data []byte, consumed int, err error) {
	br := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err = io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	consumed = len(buf) - offset - br.Len()
	return data, consumed, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	i := 0
	readVarint := func() int {
		v := 0
		shift := uint(0)
		for {
			b := delta[i]
			i++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v
	}
	srcSize := readVarint()
	tgtSize := readVarint()
	if srcSize != len(base) {
		return nil, errDeltaBaseSize
	}
	var out []byte
	for i < len(delta) {
		op := delta[i]
		i++
		if op&0x80 != 0 {
			off := 0
			size := 0
			if op&0x01 != 0 {
				off = int(delta[i])
				i++
			}
			if op&0x02 != 0 {
				off |= int(delta[i]) << 8
				i++
			}
			if op&0x04 != 0 {
				off |= int(delta[i]) << 16
				i++
			}
			if op&0x08 != 0 {
				off |= int(delta[i]) << 24
				i++
			}
			if op&0x10 != 0 {
				size = int(delta[i])
				i++
			}
			if op&0x20 != 0 {
				size |= int(delta[i]) << 8
				i++
			}
			if op&0x40 != 0 {
				size |= int(delta[i]) << 16
				i++
			}
			if size == 0 {
				size = 0x10000
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			out = append(out, delta[i:i+int(op)]...)
			i += int(op)
		} else {
			return nil, errDeltaOpcode0
		}
	}
	if len(out) != tgtSize {
		return nil, errDeltaTargetSize
	}
	return out, nil
}

type packError string

func (e packError) Error() string { return string(e) }

const (
	errBadPackMagic      = packError("bad pack magic")
	errBadPackVersion    = packError("bad pack version")
	errPackChecksum      = packError("pack checksum mismatch")
	errPackEntrySize     = packError("pack entry size mismatch")
	errUnknownPackType   = packError("unknown pack object type")
	errUnresolvedDelta   = packError("unresolved pack delta")
	errDeltaBaseSize     = packError("delta base size mismatch")
	errDeltaOpcode0      = packError("delta opcode 0")
	errDeltaTargetSize   = packError("delta target size mismatch")
)

type readPackResult struct {
	objects    map[string]*GitObject
	count      int
	deltaCount int
}

// readPack reads a pack v2/v3. resolveBase supplies thin-pack bases by oid.
func readPack(buf []byte, resolveBase func(oid string) *GitObject) (*readPackResult, error) {
	if len(buf) < 32 || string(buf[0:4]) != "PACK" {
		return nil, errBadPackMagic
	}
	version := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])
	if version != 2 && version != 3 {
		return nil, errBadPackVersion
	}
	count := int(uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11]))
	trailer := buf[len(buf)-20:]
	expect := sha1.Sum(buf[:len(buf)-20])
	if !bytes.Equal(trailer, expect[:]) {
		return nil, errPackChecksum
	}
	objects := make(map[string]*GitObject)
	offsetToOID := make(map[int]string)
	type pending struct {
		oid     string
		typ     ObjType
		data    []byte
		baseOID string
		baseOff int
		hasBase bool
		start   int
	}
	pend := []pending{}
	off := 12
	deltaCount := 0
	for n := 0; n < count; n++ {
		entryStart := off
		b := buf[off]
		off++
		typeCode := int((b >> 4) & 0x07)
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var baseOID string
		baseOff := -1
		if typeCode == 6 {
			byt := buf[off]
			off++
			base := int(byt & 0x7f)
			for byt&0x80 != 0 {
				byt = buf[off]
				off++
				base = ((base + 1) << 7) | int(byt&0x7f)
			}
			baseOff = entryStart - base
			deltaCount++
		} else if typeCode == 7 {
			baseOID = hex.EncodeToString(buf[off : off+20])
			off += 20
			deltaCount++
		}
		data, consumed, err := inflateAt(buf, off)
		if err != nil {
			return nil, err
		}
		off += consumed
		if len(data) != size {
			return nil, errPackEntrySize
		}
		if typeCode == 6 || typeCode == 7 {
			pend = append(pend, pending{typ: ObjBlob, data: data, baseOID: baseOID, baseOff: baseOff, hasBase: true, start: entryStart})
		} else {
			typ, ok := packTypes[typeCode]
			if !ok {
				return nil, errUnknownPackType
			}
			oid := hashObject(string(typ), data)
			objects[oid] = &GitObject{Type: typ, Data: data}
			offsetToOID[entryStart] = oid
			pend = append(pend, pending{oid: oid, typ: typ, data: data, start: entryStart})
		}
	}
	if deltaCount == 0 {
		return &readPackResult{objects: objects, count: count, deltaCount: 0}, nil
	}
	resolved := make(map[string]*GitObject, len(objects))
	for oid, obj := range objects {
		resolved[oid] = obj
	}
	resolveOne := func(p pending) *GitObject {
		var base *GitObject
		var baseType ObjType
		if p.baseOID != "" {
			base = resolved[p.baseOID]
			if base == nil && resolveBase != nil {
				base = resolveBase(p.baseOID)
			}
			if base != nil {
				baseType = base.Type
			}
		} else if p.baseOff >= 0 {
			if baseEntryOID, ok := offsetToOID[p.baseOff]; ok {
				base = resolved[baseEntryOID]
				if base != nil {
					baseType = base.Type
				}
			}
		}
		if base == nil {
			return nil
		}
		data, err := applyDelta(base.Data, p.data)
		if err != nil {
			return nil
		}
		return &GitObject{Type: baseType, Data: data}
	}
	var remaining []pending
	for _, p := range pend {
		if p.hasBase {
			remaining = append(remaining, p)
		}
	}
	for pass := 0; pass < 10 && len(remaining) > 0; pass++ {
		next := []pending{}
		for _, p := range remaining {
			r := resolveOne(p)
			if r == nil {
				next = append(next, p)
				continue
			}
			oid := hashObject(string(r.Type), r.Data)
			resolved[oid] = r
			offsetToOID[p.start] = oid
		}
		if len(next) == len(remaining) {
			break
		}
		remaining = next
	}
	if len(remaining) > 0 {
		return nil, errUnresolvedDelta
	}
	return &readPackResult{objects: resolved, count: count, deltaCount: deltaCount}, nil
}

// Adler-32 (RFC 1950) with deferred modulus.
func adler32(data []byte) uint32 {
	a := uint32(1)
	b := uint32(0)
	i := 0
	for i < len(data) {
		end := i + 5552
		if end > len(data) {
			end = len(data)
		}
		for i < end {
			a += uint32(data[i])
			i++
			b += a
		}
		a %= 65521
		b %= 65521
	}
	return (b << 16) | a
}

// deflateStoredParts appends a zlib stream for data using RFC 1951 stored
// (uncompressed) DEFLATE blocks: zero compression work, 100% valid.
func deflateStoredParts(data []byte, parts *[][]byte) {
	*parts = append(*parts, []byte{0x78, 0x01})
	if len(data) == 0 {
		h := make([]byte, 5)
		h[0] = 0x01
		h[1] = 0
		h[2] = 0
		h[3] = 0xff
		h[4] = 0xff
		*parts = append(*parts, h)
	} else {
		off := 0
		for off < len(data) {
			chunk := 65535
			if len(data)-off < chunk {
				chunk = len(data) - off
			}
			last := off+chunk >= len(data)
			h := make([]byte, 5)
			if last {
				h[0] = 0x01
			} else {
				h[0] = 0x00
			}
			h[1] = byte(chunk)
			h[2] = byte(chunk >> 8)
			nlen := 0xffff ^ chunk
			h[3] = byte(nlen)
			h[4] = byte(nlen >> 8)
			*parts = append(*parts, h, data[off:off+chunk])
			off += chunk
		}
	}
	trailer := make([]byte, 4)
	sum := adler32(data)
	trailer[0] = byte(sum >> 24)
	trailer[1] = byte(sum >> 16)
	trailer[2] = byte(sum >> 8)
	trailer[3] = byte(sum)
	*parts = append(*parts, trailer)
}

// writePack writes a pack v2 of plain (undelted) objects, deterministic order (oid sort).
func writePack(objs []*GitObject) []byte {
	type keyed struct {
		oid string
		obj *GitObject
	}
	keyedList := make([]keyed, 0, len(objs))
	for _, obj := range objs {
		keyedList = append(keyedList, keyed{oid: hashObject(string(obj.Type), obj.Data), obj: obj})
	}
	sort.Slice(keyedList, func(i, j int) bool { return keyedList[i].oid < keyedList[j].oid })
	parts := [][]byte{}
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	head[4] = 0
	head[5] = 0
	head[6] = 0
	head[7] = 2
	n := len(keyedList)
	head[8] = byte(n >> 24)
	head[9] = byte(n >> 16)
	head[10] = byte(n >> 8)
	head[11] = byte(n)
	parts = append(parts, head)
	for _, k := range keyedList {
		obj := k.obj
		typeCode := typeCodes[obj.Type]
		size := len(obj.Data)
		headerBytes := []byte{}
		s := size
		first := (typeCode << 4) | (s & 0x0f)
		s >>= 4
		if s > 0 {
			first |= 0x80
		}
		headerBytes = append(headerBytes, byte(first))
		for s > 0 {
			b := s & 0x7f
			s >>= 7
			if s > 0 {
				b |= 0x80
			}
			headerBytes = append(headerBytes, byte(b))
		}
		parts = append(parts, headerBytes)
		deflateStoredParts(obj.Data, &parts)
	}
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	trailer := sha1.Sum(body)
	return append(body, trailer[:]...)
}

// ---------------------------------------------------------------- fetch set computation

type fetchPlan struct {
	objects   map[string]*GitObject
	shallow   []string
	unshallow []string
}

// planFetch computes the objects to send for wants, given haves and a depth limit.
func planFetch(store *Store, repo *Repo, wants, haves []string, depth *int, prevShallow []string) fetchPlan {
	haveReachable := make(map[string]bool)
	if len(haves) > 0 {
		stopAt := make(map[string]bool)
		for _, s := range prevShallow {
			stopAt[s] = true
		}
		haveObjs := store.ReachableObjects(repo, haves, stopAt)
		for oid := range haveObjs {
			haveReachable[oid] = true
		}
	}
	send := make(map[string]*GitObject)
	shallowSet := make(map[string]bool)
	unshallowSet := make(map[string]bool)
	dist := make(map[string]int)
	queue := []string{}
	for _, w := range wants {
		if wobj := repo.Objects[w]; wobj != nil && wobj.Type == ObjTag {
			if !haveReachable[w] {
				send[w] = wobj
			}
			if m := tagTarget(wobj.Data); m != "" {
				if _, ok := dist[m]; !ok {
					dist[m] = 0
					queue = append(queue, m)
				}
			}
			continue
		}
		if _, ok := dist[w]; !ok {
			dist[w] = 0
			queue = append(queue, w)
		}
	}
	includeCommit := func(oid string) {
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			return
		}
		if !haveReachable[oid] {
			send[oid] = obj
		}
		c := store.ParseCommit(obj.Data)
		treeObjs := store.ReachableObjects(repo, []string{c.Tree}, nil)
		for toid, tobj := range treeObjs {
			if tobj.Type == ObjCommit {
				continue
			}
			if !haveReachable[toid] {
				send[toid] = tobj
			}
		}
	}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		d := dist[oid]
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		includeCommit(oid)
		isBoundary := depth != nil && d >= *depth-1
		if isBoundary {
			shallowSet[oid] = true
			continue
		}
		c := store.ParseCommit(obj.Data)
		for _, p := range c.Parents {
			if _, ok := dist[p]; !ok {
				dist[p] = d + 1
				queue = append(queue, p)
			}
		}
	}
	// Tags pointing at wanted commits.
	wantSet := make(map[string]bool)
	for _, w := range wants {
		wantSet[w] = true
	}
	for _, ref := range sortedRefNames(repo) {
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		oid := repo.Refs[ref]
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjTag {
			continue
		}
		if m := tagTarget(obj.Data); m != "" && wantSet[m] && !haveReachable[oid] {
			send[oid] = obj
		}
	}
	for _, s := range prevShallow {
		if _, ok := dist[s]; ok && !shallowSet[s] {
			if obj := repo.Objects[s]; obj != nil && obj.Type == ObjCommit {
				c := store.ParseCommit(obj.Data)
				parentsIncluded := true
				for _, p := range c.Parents {
					if _, ok := dist[p]; !ok && !haveReachable[p] {
						parentsIncluded = false
						break
					}
				}
				if parentsIncluded {
					unshallowSet[s] = true
				}
			}
		}
	}
	shallow := []string{}
	for s := range shallowSet {
		shallow = append(shallow, s)
	}
	sort.Strings(shallow)
	unshallow := []string{}
	for s := range unshallowSet {
		unshallow = append(unshallow, s)
	}
	sort.Strings(unshallow)
	return fetchPlan{objects: send, shallow: shallow, unshallow: unshallow}
}

func sortedRefNames(repo *Repo) []string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedRefs(repo *Repo) [][2]string {
	names := sortedRefNames(repo)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, repo.Refs[name]})
	}
	return out
}

// ---------------------------------------------------------------- sideband

// sidebandWrap wraps bytes into sideband-64k pkt-lines (band 1 = data).
func sidebandWrap(data []byte, band byte) []byte {
	parts := [][]byte{}
	const chunk = 65516 - 1
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		slice := data[i:end]
		payload := make([]byte, 0, len(slice)+1)
		payload = append(payload, band)
		payload = append(payload, slice...)
		parts = append(parts, pktLine(payload))
	}
	if len(data) == 0 {
		parts = append(parts, pktLine([]byte{band}))
	}
	parts = append(parts, pktFlush)
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---------------------------------------------------------------- advertisements

func peeledOID(repo *Repo, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != ObjTag {
		return ""
	}
	return tagTarget(obj.Data)
}

// advertiseV0 is the v0 advertisement body for a service.
func advertiseV0(store *Store, repo *Repo, service string) []byte {
	parts := [][]byte{}
	parts = append(parts, pktLineStr("# service="+service+"\n"))
	parts = append(parts, pktFlush)
	caps := "side-band-64k thin-pack ofs-delta multi_ack"
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs ofs-delta"
	}
	headTarget := "refs/heads/" + repo.DefaultBranch
	headOID := repo.Refs[headTarget]
	if headOID == "" {
		headOID = ZeroOID
	}
	headLine := headOID + " HEAD\x00" + caps + " symref=HEAD:" + headTarget + " agent=forge/1.0\n"
	parts = append(parts, pktLineStr(headLine))
	for _, r := range sortedRefs(repo) {
		parts = append(parts, pktLineStr(r[1]+" "+r[0]+"\n"))
		if peeled := peeledOID(repo, r[1]); peeled != "" {
			parts = append(parts, pktLineStr(peeled+" "+r[0]+"^{}\n"))
		}
	}
	parts = append(parts, pktFlush)
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var advertiseV2Cache []byte

// advertiseV2 is the v2 advertisement body (upload-pack). Constant.
func advertiseV2() []byte {
	if advertiseV2Cache != nil {
		return advertiseV2Cache
	}
	parts := [][]byte{
		pktLineStr("version 2\n"),
		pktLineStr("ls-refs=unborn\n"),
		pktLineStr("fetch=shallow\n"),
		pktLineStr("object-format=sha1\n"),
		pktLineStr("agent=forge/1.0\n"),
		pktFlush,
	}
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	advertiseV2Cache = out
	return out
}

// advertisedV0 is the v0 advertisement, memoized under the wire fingerprint.
func advertisedV0(store *Store, repo *Repo, service string) []byte {
	key := "v0-adv:" + wireFingerprint(store, repo) + ":" + service
	if hit, ok := wireGet(repo, key); ok {
		return hit
	}
	answer := advertiseV0(store, repo, service)
	wireSet(repo, key, answer)
	return answer
}

// ---------------------------------------------------------------- upload-pack

type lsRefEntry struct {
	oid          string
	name         string
	symrefTarget string
	peeled       string
	unborn       bool
}

func handleLsRefs(store *Store, repo *Repo, args []string) []byte {
	peel := false
	symrefs := false
	prefixes := []string{}
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	out := []lsRefEntry{}
	headTarget := "refs/heads/" + repo.DefaultBranch
	headOID, hasHead := repo.Refs[headTarget]
	matchPrefix := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if hasHead && matchPrefix("HEAD") {
		entry := lsRefEntry{oid: headOID, name: "HEAD"}
		if symrefs {
			entry.symrefTarget = headTarget
		}
		out = append(out, entry)
	} else if !hasHead && matchPrefix("HEAD") {
		out = append(out, lsRefEntry{name: "HEAD", symrefTarget: headTarget, unborn: true})
	}
	for _, r := range sortedRefs(repo) {
		if !matchPrefix(r[0]) {
			continue
		}
		entry := lsRefEntry{oid: r[1], name: r[0]}
		if peel {
			if p := peeledOID(repo, r[1]); p != "" {
				entry.peeled = p
			}
		}
		out = append(out, entry)
	}
	parts := [][]byte{}
	for _, r := range out {
		if r.unborn {
			line := "unborn HEAD"
			if r.symrefTarget != "" {
				line += " symref-target:" + r.symrefTarget
			}
			parts = append(parts, pktLineStr(line+"\n"))
		} else {
			line := r.oid + " " + r.name
			if r.symrefTarget != "" {
				line += " symref-target:" + r.symrefTarget
			}
			if r.peeled != "" {
				line += " peeled:" + r.peeled
			}
			parts = append(parts, pktLineStr(line+"\n"))
		}
	}
	parts = append(parts, pktFlush)
	var res []byte
	for _, p := range parts {
		res = append(res, p...)
	}
	return res
}

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	deepen   *int
	thinPack bool
}

func parseV2Fetch(body []byte) fetchArgs {
	items, _, _ := parsePktLines(body)
	args := fetchArgs{}
	for _, it := range items {
		if it.kind != pktLineKind {
			continue
		}
		line := strings.TrimRight(string(it.data), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			args.wants = append(args.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(line[7:])
			args.deepen = &n
		case line == "thin-pack":
			args.thinPack = true
		}
	}
	return args
}

func parseV0Fetch(body []byte) fetchArgs {
	items, _, _ := parsePktLines(body)
	args := fetchArgs{}
	for _, it := range items {
		if it.kind != pktLineKind {
			continue
		}
		line := strings.TrimRight(string(it.data), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimSpace(line[5:])
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				args.wants = append(args.wants, fields[0])
			}
			if strings.Contains(rest, "thin-pack") {
				args.thinPack = true
			}
		case strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(line[7:])
			args.deepen = &n
		}
	}
	return args
}

func fetchResponseV2(store *Store, repo *Repo, args fetchArgs) []byte {
	plan := planFetch(store, repo, args.wants, args.haves, args.deepen, args.shallow)
	objs := make([]*GitObject, 0, len(plan.objects))
	for _, obj := range plan.objects {
		objs = append(objs, obj)
	}
	pack := writePack(objs)
	parts := [][]byte{}
	if len(args.haves) > 0 {
		known := []string{}
		for _, h := range args.haves {
			if _, ok := repo.Objects[h]; ok {
				known = append(known, h)
			}
		}
		parts = append(parts, pktLineStr("acknowledgments\n"))
		if len(known) > 0 {
			for _, h := range known {
				parts = append(parts, pktLineStr("ACK "+h+"\n"))
			}
			parts = append(parts, pktLineStr("ready\n"))
		} else {
			parts = append(parts, pktLineStr("NAK\n"))
		}
		parts = append(parts, pktDelim)
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		parts = append(parts, pktLineStr("shallow-info\n"))
		for _, s := range plan.shallow {
			parts = append(parts, pktLineStr("shallow "+s+"\n"))
		}
		for _, s := range plan.unshallow {
			parts = append(parts, pktLineStr("unshallow "+s+"\n"))
		}
		parts = append(parts, pktDelim)
	}
	parts = append(parts, pktLineStr("packfile\n"))
	parts = append(parts, sidebandWrap(pack, 1))
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func fetchResponseV0(store *Store, repo *Repo, args fetchArgs) []byte {
	plan := planFetch(store, repo, args.wants, args.haves, args.deepen, args.shallow)
	objs := make([]*GitObject, 0, len(plan.objects))
	for _, obj := range plan.objects {
		objs = append(objs, obj)
	}
	pack := writePack(objs)
	parts := [][]byte{}
	if len(args.haves) > 0 {
		known := []string{}
		for _, h := range args.haves {
			if _, ok := repo.Objects[h]; ok {
				known = append(known, h)
			}
		}
		if len(known) > 0 {
			for _, h := range known {
				parts = append(parts, pktLineStr("ACK "+h+"\n"))
			}
		} else {
			parts = append(parts, pktLineStr("NAK\n"))
		}
	} else {
		parts = append(parts, pktLineStr("NAK\n"))
	}
	for _, s := range plan.shallow {
		parts = append(parts, pktLineStr("shallow "+s+"\n"))
	}
	for _, s := range plan.unshallow {
		parts = append(parts, pktLineStr("unshallow "+s+"\n"))
	}
	parts = append(parts, sidebandWrap(pack, 1))
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type gitResult struct {
	status      int
	body        []byte
	contentType string
}

// handleUploadPack handles POST git-upload-pack. isV2 from Git-Protocol header.
func handleUploadPack(store *Store, repo *Repo, body []byte, isV2 bool) gitResult {
	ct := "application/x-git-upload-pack-result"
	if len(body) == 0 {
		return gitResult{status: 200, body: []byte{}, contentType: ct}
	}
	if isV2 {
		items, _, _ := parsePktLines(body)
		command := ""
		argLines := []string{}
		seenDelim := false
		for _, it := range items {
			if it.kind != pktLineKind {
				if it.kind == pktDelimKind {
					seenDelim = true
				}
				continue
			}
			line := strings.TrimRight(string(it.data), "\n")
			if !seenDelim && strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			} else if seenDelim {
				argLines = append(argLines, line)
			} else if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
		}
		if command == "ls-refs" {
			key := "v2-ls:" + wireFingerprint(store, repo) + ":" + jsonMarshalStrings(argLines)
			if hit, ok := wireGet(repo, key); ok {
				return gitResult{status: 200, body: hit, contentType: ct}
			}
			answer := handleLsRefs(store, repo, argLines)
			wireSet(repo, key, answer)
			return gitResult{status: 200, body: answer, contentType: ct}
		}
		if command == "fetch" {
			args := parseV2Fetch(body)
			if len(args.wants) == 0 {
				return gitResult{status: 200, body: concatBytes(pktLineStr("acknowledgments\n"), pktLineStr("NAK\n"), pktFlush), contentType: ct}
			}
			key := "v2-fetch:" + wireFingerprint(store, repo) + ":" + fetchArgsKey(args)
			if hit, ok := wireGet(repo, key); ok {
				return gitResult{status: 200, body: hit, contentType: ct}
			}
			answer := fetchResponseV2(store, repo, args)
			wireSet(repo, key, answer)
			return gitResult{status: 200, body: answer, contentType: ct}
		}
		return gitResult{status: 200, body: []byte("0000"), contentType: ct}
	}
	args := parseV0Fetch(body)
	if len(args.wants) == 0 {
		return gitResult{status: 200, body: concatBytes(pktLineStr("NAK\n"), pktFlush), contentType: ct}
	}
	key := "v0-fetch:" + wireFingerprint(store, repo) + ":" + fetchArgsKey(args)
	if hit, ok := wireGet(repo, key); ok {
		return gitResult{status: 200, body: hit, contentType: ct}
	}
	answer := fetchResponseV0(store, repo, args)
	wireSet(repo, key, answer)
	return gitResult{status: 200, body: answer, contentType: ct}
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func jsonMarshalStrings(items []string) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(JString(s))
	}
	sb.WriteByte(']')
	return sb.String()
}

func fetchArgsKey(args fetchArgs) string {
	var sb strings.Builder
	sb.WriteString("{\"wants\":")
	sb.WriteString(jsonMarshalStrings(args.wants))
	sb.WriteString(",\"haves\":")
	sb.WriteString(jsonMarshalStrings(args.haves))
	sb.WriteString(",\"shallow\":")
	sb.WriteString(jsonMarshalStrings(args.shallow))
	sb.WriteString(",\"deepen\":")
	if args.deepen == nil {
		sb.WriteString("null")
	} else {
		sb.WriteString(strconv.Itoa(*args.deepen))
	}
	sb.WriteString(",\"thinPack\":")
	if args.thinPack {
		sb.WriteString("true")
	} else {
		sb.WriteString("false")
	}
	sb.WriteString("}")
	return sb.String()
}

// ---------------------------------------------------------------- wire cache

// wireCache holds memoized wire answers per repo, capped at 64 entries.
var wireCaches = make(map[*Repo]map[string][]byte)
var fpCaches = make(map[*Repo]fpEntry)

type fpEntry struct {
	version int
	fp      string
}

func wireFingerprint(store *Store, repo *Repo) string {
	if hit, ok := fpCaches[repo]; ok && hit.version == store.Version {
		return hit.fp
	}
	refs := []string{}
	for name, oid := range repo.Refs {
		refs = append(refs, name+"="+oid)
	}
	sort.Strings(refs)
	fp := strings.Join(refs, ",") + "|" + strconv.Itoa(len(repo.Objects))
	fpCaches[repo] = fpEntry{version: store.Version, fp: fp}
	return fp
}

func wireGet(repo *Repo, key string) ([]byte, bool) {
	per, ok := wireCaches[repo]
	if !ok {
		return nil, false
	}
	v, ok := per[key]
	return v, ok
}

func wireSet(repo *Repo, key string, body []byte) {
	per, ok := wireCaches[repo]
	if !ok {
		per = make(map[string][]byte)
		wireCaches[repo] = per
	}
	if len(per) >= 64 {
		for k := range per {
			delete(per, k)
			break
		}
	}
	per[key] = body
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
	caps   string
}

// parseReceivePack parses a receive-pack request: pkt-line commands, flush, then raw pack bytes.
func parseReceivePack(body []byte) (commands []pushCommand, pack []byte) {
	items, rest, _ := parsePktLines(body)
	for _, it := range items {
		if it.kind != pktLineKind {
			continue
		}
		line := string(it.data)
		caps := ""
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = line[nul+1:]
			line = line[:nul]
		}
		parts := strings.Split(strings.TrimRight(line, "\n"), " ")
		if len(parts) >= 3 {
			commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2], caps: strings.TrimSpace(caps)})
		}
	}
	return commands, rest
}

func handleReceivePack(store *Store, repo *Repo, body []byte) gitResult {
	ct := "application/x-git-receive-pack-result"
	commands, pack := parseReceivePack(body)
	unpackOK := true
	unpackDetail := ""
	packObjects := make(map[string]*GitObject)
	if len(pack) > 0 {
		parsed, err := readPack(pack, func(oid string) *GitObject {
			if o := repo.Objects[oid]; o != nil {
				return o
			}
			return packObjects[oid]
		})
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for oid, obj := range parsed.objects {
				packObjects[oid] = obj
			}
		}
	}
	lookup := func(oid string) *GitObject {
		if o := repo.Objects[oid]; o != nil {
			return o
		}
		return packObjects[oid]
	}
	isAncestor2 := func(ancestor, oid string) bool {
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
			obj := lookup(cur)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			stack = append(stack, store.ParseCommit(obj.Data).Parents...)
		}
		return false
	}
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	results := []result{}
	for _, cmd := range commands {
		current, hasCurrent := repo.Refs[cmd.ref]
		ok := false
		reason := ""
		switch {
		case !unpackOK:
			ok = false
			reason = "unpack failed"
		case cmd.newOID == ZeroOID:
			if !hasCurrent {
				ok = false
				reason = "ref does not exist"
			} else if current != cmd.oldOID {
				ok = false
				reason = "non-fast-forward"
			} else {
				ok = true
			}
		case cmd.oldOID == ZeroOID:
			if hasCurrent {
				ok = false
				reason = "non-fast-forward"
			} else if lookup(cmd.newOID) == nil {
				ok = false
				reason = "missing object"
			} else {
				ok = true
			}
		default:
			if !hasCurrent {
				ok = false
				reason = "ref does not exist"
			} else if current != cmd.oldOID {
				ok = false
				reason = "non-fast-forward"
			} else if lookup(cmd.newOID) == nil {
				ok = false
				reason = "missing object"
			} else if !isAncestor2(cmd.oldOID, cmd.newOID) {
				ok = false
				reason = "non-fast-forward"
			} else {
				ok = true
			}
		}
		if ok {
			for oid, obj := range packObjects {
				if _, exists := repo.Objects[oid]; !exists {
					repo.Objects[oid] = obj
				}
			}
			if cmd.newOID == ZeroOID {
				delete(repo.Refs, cmd.ref)
			} else {
				repo.Refs[cmd.ref] = cmd.newOID
			}
		}
		results = append(results, result{ref: cmd.ref, ok: ok, reason: reason})
	}
	reportParts := [][]byte{}
	if unpackOK {
		reportParts = append(reportParts, pktLineStr("unpack ok\n"))
	} else {
		reportParts = append(reportParts, pktLineStr("unpack "+unpackDetail+"\n"))
	}
	for _, r := range results {
		if r.ok {
			reportParts = append(reportParts, pktLineStr("ok "+r.ref+"\n"))
		} else {
			reportParts = append(reportParts, pktLineStr("ng "+r.ref+" "+r.reason+"\n"))
		}
	}
	reportParts = append(reportParts, pktFlush)
	var report []byte
	for _, p := range reportParts {
		report = append(report, p...)
	}
	return gitResult{status: 200, body: sidebandWrap(report, 1), contentType: ct}
}
