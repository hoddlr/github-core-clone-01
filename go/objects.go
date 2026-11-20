// Package main: object model, pkt-line codec and packfile codec for the forge.
//
// The git wire format is implemented here from the protocol as the suite's
// client speaks it (program/env/src/git-client.ts): pkt-lines, SHA-1 object
// ids, plain packfiles, OFS/REF deltas on read. No third-party code is used.
package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// -------------------------------------------------------------------------------------------
// object ids and object model
// -------------------------------------------------------------------------------------------

const (
	objCommit = "commit"
	objTree   = "tree"
	objBlob   = "blob"
	objTag    = "tag"
)

// Object is one git object: its type and its exact bytes (no loose header).
type Object struct {
	Type string
	Data []byte
}

// ZeroOID is the all-zero object id.
const ZeroOID = "0000000000000000000000000000000000000000"

// IsOID reports whether s is a lowercase 40-hex object id.
func IsOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HashObject returns git's object id: SHA-1 over "<type> <size>\0" + data.
func HashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree object into its entries, in stored order.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		mode := string(data[off:sp])
		for _, c := range mode {
			if c < '0' || c > '7' {
				return nil, fmt.Errorf("tree entry mode %q is not octal", mode)
			}
		}
		entries = append(entries, TreeEntry{
			Mode: mode,
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries, nil
}

// ParseCommit returns the tree id and parent ids of a commit object.
func ParseCommit(data []byte) (tree string, parents []string, err error) {
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
	}
	for _, line := range strings.Split(string(head), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		key := line
		val := ""
		if sp >= 0 {
			key, val = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			tree = val
		case "parent":
			parents = append(parents, val)
		}
	}
	if !IsOID(tree) {
		return "", nil, errors.New("commit has no valid tree header")
	}
	for _, p := range parents {
		if !IsOID(p) {
			return "", nil, errors.New("commit has a bad parent id")
		}
	}
	return tree, parents, nil
}

// ParseTag returns the object, type and tag name of an annotated tag object.
func ParseTag(data []byte) (object, typ, tag string, err error) {
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(head), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			return "", "", "", fmt.Errorf("tag header %q has no value", line)
		}
		headers[line[:sp]] = line[sp+1:]
	}
	object = headers["object"]
	typ = headers["type"]
	tag = headers["tag"]
	if !IsOID(object) {
		return "", "", "", errors.New("tag has no valid object header")
	}
	switch typ {
	case objCommit, objTree, objBlob, objTag:
	default:
		return "", "", "", fmt.Errorf("tag has bad type %q", typ)
	}
	return object, typ, tag, nil
}

// CollectReachable returns every object id reachable from roots (commit ->
// tree + parents, tag -> target, tree -> entries, gitlinks skipped).
// lookup returns nil for an unknown id; missing roots are reported.
func CollectReachable(roots []string, lookup func(string) *Object) (map[string]bool, string) {
	seen := map[string]bool{}
	var queue []string
	for _, r := range roots {
		if IsOID(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			return nil, oid
		}
		seen[oid] = true
		switch obj.Type {
		case objCommit:
			tree, parents, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, oid
			}
			queue = append(queue, tree)
			queue = append(queue, parents...)
		case objTag:
			target, _, _, err := ParseTag(obj.Data)
			if err != nil {
				return nil, oid
			}
			queue = append(queue, target)
		case objTree:
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil, oid
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return seen, ""
}

// IsDescendant reports whether newOID is oldOID or descends from it through
// commit parents. Both must be commit objects in the store.
func IsDescendant(newOID, oldOID string, lookup func(string) *Object) bool {
	if newOID == oldOID {
		return true
	}
	seen := map[string]bool{}
	queue := []string{newOID}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != objCommit {
			continue
		}
		_, parents, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range parents {
			if p == oldOID {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// PeelToCommit follows annotated tags until a commit (or a non-tag object).
func PeelToCommit(oid string, lookup func(string) *Object) string {
	for i := 0; i < 16; i++ {
		obj := lookup(oid)
		if obj == nil || obj.Type != objTag {
			return oid
		}
		target, _, _, err := ParseTag(obj.Data)
		if err != nil {
			return oid
		}
		oid = target
	}
	return oid
}

// -------------------------------------------------------------------------------------------
// pkt-line codec
// -------------------------------------------------------------------------------------------

const pktLineMax = 65520

// pktLine encodes one data packet: 4 hex length bytes then the payload.
func pktLine(payload []byte) []byte {
	if len(payload) > pktLineMax-4 {
		panic(fmt.Sprintf("pkt-line payload is %d bytes, over the maximum", len(payload)))
	}
	out := make([]byte, 4+len(payload))
	// Write the length as 4 lowercase hex digits.
	l := len(payload) + 4
	out[0] = hexDigit(l >> 12)
	out[1] = hexDigit((l >> 8) & 0xf)
	out[2] = hexDigit((l >> 4) & 0xf)
	out[3] = hexDigit(l & 0xf)
	copy(out[4:], payload)
	return out
}

func hexDigit(n int) byte {
	if n < 10 {
		return byte('0' + n)
	}
	return byte('a' + n - 10)
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// pktKind classifies one packet header.
const (
	pktData = iota
	pktFlushKind
	pktDelimKind
	pktResponseEndKind
)

// pktReader reads pkt-lines sequentially from a byte slice.
type pktReader struct {
	b   []byte
	off int
}

// next returns the next packet's payload and kind, or an error on malformed input.
func (r *pktReader) next() (payload []byte, kind int, err error) {
	if r.off+4 > len(r.b) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	head := r.b[r.off : r.off+4]
	for _, c := range head {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return nil, 0, fmt.Errorf("invalid pkt-line length %q", string(head))
		}
	}
	length := 0
	for _, c := range head {
		length *= 16
		switch {
		case c >= '0' && c <= '9':
			length += int(c - '0')
		case c >= 'a' && c <= 'f':
			length += int(c-'a') + 10
		default:
			length += int(c-'A') + 10
		}
	}
	switch length {
	case 0:
		r.off += 4
		return nil, pktFlushKind, nil
	case 1:
		r.off += 4
		return nil, pktDelimKind, nil
	case 2:
		r.off += 4
		return nil, pktResponseEndKind, nil
	case 3:
		return nil, 0, errors.New("invalid pkt-line length 0003")
	}
	if length > pktLineMax {
		return nil, 0, fmt.Errorf("pkt-line length %d exceeds %d", length, pktLineMax)
	}
	if r.off+length > len(r.b) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	payload = r.b[r.off+4 : r.off+length]
	r.off += length
	return payload, pktData, nil
}

// rest returns the unread remainder of the input.
func (r *pktReader) rest() []byte { return r.b[r.off:] }

// -------------------------------------------------------------------------------------------
// zlib inflate with consumed length
// -------------------------------------------------------------------------------------------

// inflateAt inflates one zlib stream starting at off and returns its bytes and
// the offset just past the stream (header + deflate data + adler32).
func inflateAt(b []byte, off int) (data []byte, next int, err error) {
	if off >= len(b) {
		return nil, 0, errors.New("zlib stream starts past the end")
	}
	rest := b[off:]
	ur := bytes.NewReader(rest)
	br := bufio.NewReader(ur)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %w", off, err)
	}
	data, err = io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %w", off, err)
	}
	consumed := len(rest) - ur.Len() - br.Buffered()
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", off)
	}
	return data, off + consumed, nil
}

// deflate returns the zlib-compressed form of data at the default level.
// Go's compressor is deterministic for fixed input and level.
func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// -------------------------------------------------------------------------------------------
// delta application
// -------------------------------------------------------------------------------------------

// applyDelta applies a git delta instruction stream to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			by := delta[off]
			off++
			value += int(by&0x7f) << shift
			shift += 7
			if by&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
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
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff, copySize := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOff += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if off+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[off:off+literal])
			off += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

// -------------------------------------------------------------------------------------------
// packfile read and write (version 2, plain objects plus OFS/REF deltas on read)
// -------------------------------------------------------------------------------------------

var packTypeCode = map[string]int{objCommit: 1, objTree: 2, objBlob: 3, objTag: 4}
var packCodeType = map[int]string{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}

// packChecksumOK verifies a pack's trailer SHA-1.
func packChecksumOK(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(sum[:], pack[len(pack)-20:])
}

type rawPackEntry struct {
	offset   int
	typeCode int
	size     int
	data     []byte
	baseOff  int // OFS_DELTA: absolute offset of the base entry, -1 when n/a
	baseOID  string
}

// ReadPack parses a packfile and resolves every object, including deltas.
// resolveBase supplies a base object a thin pack leaves out (the store).
func ReadPack(pack []byte, resolveBase func(string) *Object) (map[string]*Object, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	if !packChecksumOK(pack) {
		return nil, errors.New("pack trailer does not match its bytes")
	}
	raw := make([]*rawPackEntry, 0, count)
	byOffset := map[int]*rawPackEntry{}
	off := 12
	for i := 0; i < count; i++ {
		entryOff := off
		if off >= len(pack) {
			return nil, errors.New("pack entry has no size header")
		}
		by := pack[off]
		first := by
		size := int(by & 0x0f)
		shift := uint(4)
		off++
		for by&0x80 != 0 {
			if off >= len(pack) {
				return nil, errors.New("pack entry size varint is truncated")
			}
			by = pack[off]
			off++
			size += int(by&0x7f) << shift
			shift += 7
			if shift > 63 {
				return nil, errors.New("pack entry size varint is too long")
			}
		}
		typeCode := int((first >> 4) & 0x07)
		entry := &rawPackEntry{offset: entryOff, typeCode: typeCode, size: size, baseOff: -1}
		switch typeCode {
		case 6: // OFS_DELTA
			if off >= len(pack) {
				return nil, errors.New("ofs-delta has no base offset")
			}
			by = pack[off]
			off++
			value := int(by & 0x7f)
			for by&0x80 != 0 {
				if off >= len(pack) {
					return nil, errors.New("ofs-delta base offset is truncated")
				}
				by = pack[off]
				off++
				value = ((value + 1) << 7) + int(by&0x7f)
			}
			if value <= 0 || value > entryOff {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOff, value)
			}
			entry.baseOff = entryOff - value
		case 7: // REF_DELTA
			if off+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		default:
			if _, ok := packCodeType[typeCode]; !ok {
				return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
			}
		}
		data, next, err := inflateAt(pack[:trailerStart], off)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOff, size, len(data))
		}
		entry.data = data
		off = next
		raw = append(raw, entry)
		byOffset[entryOff] = entry
	}
	if off != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-off)
	}

	objects := map[string]*Object{}
	resolvedByOID := map[string]*rawPackEntry{}
	resolved := map[int]*Object{}
	resolving := map[int]bool{}
	var resolve func(e *rawPackEntry) (*Object, error)
	resolve = func(e *rawPackEntry) (*Object, error) {
		if done, ok := resolved[e.offset]; ok {
			return done, nil
		}
		if resolving[e.offset] {
			return nil, fmt.Errorf("delta cycle at pack offset %d", e.offset)
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			baseEntry, ok := byOffset[e.baseOff]
			if !ok {
				return nil, fmt.Errorf("ofs-delta base at offset %d is not in the pack", e.baseOff)
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			typ = base.Type
			data, err = applyDelta(base.Data, e.data)
			if err != nil {
				return nil, err
			}
		case 7:
			if baseEntry, ok := resolvedByOID[e.baseOID]; ok {
				base, err := resolve(baseEntry)
				if err != nil {
					return nil, err
				}
				typ = base.Type
				data, err = applyDelta(base.Data, e.data)
				if err != nil {
					return nil, err
				}
			} else if resolveBase != nil {
				ext := resolveBase(e.baseOID)
				if ext == nil {
					return nil, fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", e.baseOID)
				}
				typ = ext.Type
				var err error
				data, err = applyDelta(ext.Data, e.data)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", e.baseOID)
			}
		default:
			typ = packCodeType[e.typeCode]
			data = e.data
		}
		obj := &Object{Type: typ, Data: data}
		oid := HashObject(typ, data)
		resolved[e.offset] = obj
		resolvedByOID[oid] = e
		objects[oid] = obj
		return obj, nil
	}
	for _, e := range raw {
		if _, err := resolve(e); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

// writePackObjectHeader encodes one object's pack header (type/size varint).
func writePackObjectHeader(typ string, size int) []byte {
	code := packTypeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining >>= 4
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining >>= 7
	}
	out = append(out, byte(first))
	return out
}

// WritePack builds a version 2 packfile from plain objects (no deltas).
// Objects are ordered by id, so the bytes are deterministic.
func WritePack(objs map[string]*Object) []byte {
	oids := make([]string, 0, len(objs))
	for oid := range objs {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	return writePackSorted(objs, oids)
}

func writePackSorted(objs map[string]*Object, oids []string) []byte {
	capacity := 12 + len(objs)*20
	for _, oid := range oids {
		capacity += len(objs[oid].Data)
	}
	var body bytes.Buffer
	body.Grow(capacity)
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	head[4], head[5], head[6], head[7] = 0, 0, 0, 2
	head[8] = byte(len(oids) >> 24)
	head[9] = byte(len(oids) >> 16)
	head[10] = byte(len(oids) >> 8)
	head[11] = byte(len(oids))
	body.Write(head)
	var compressor *zlib.Writer
	for _, oid := range oids {
		obj := objs[oid]
		body.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		if compressor == nil {
			compressor, _ = zlib.NewWriterLevel(&body, zlib.BestSpeed)
		} else {
			compressor.Reset(&body)
		}
		compressor.Write(obj.Data)
		compressor.Close()
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// EmptyPack is a valid version 2 packfile holding zero objects.
func EmptyPack() []byte {
	return WritePack(map[string]*Object{})
}
