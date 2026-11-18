// Package main: git smart HTTP (protocol v2 upload-pack, v0 receive-pack).
//
// Implemented from the wire the suite's client speaks
// (program/env/src/git-client.ts): v2 advertisement + ls-refs + fetch with
// shallow/deepen, v0 ref advertisement, receive-pack with report-status and
// side-band-64k, fast-forward-only updates. Packs are plain (no deltas are
// ever sent); incoming packs may carry OFS/REF deltas and are resolved on
// read (see objects.go).
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// -------------------------------------------------------------------------------------------
// advertisements
// -------------------------------------------------------------------------------------------

// v2Advertisement is the protocol v2 capability advertisement: `version 2`
// followed by one capability line each, then a flush.
func v2Advertisement() []byte {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("version 2\n")))
	buf.Write(pktLine([]byte("agent=" + forgeAgent + "\n")))
	buf.Write(pktLine([]byte("ls-refs=unborn\n")))
	buf.Write(pktLine([]byte("fetch=shallow\n")))
	buf.Write(pktLine([]byte("object-format=sha1\n")))
	buf.Write(pktFlush())
	return buf.Bytes()
}

// v0Advertisement is the v0 ref advertisement for a service: the service
// preamble, a flush, then one line per ref (peeled tags follow their tag),
// then a flush. An empty repository advertises only the capabilities^{}
// pseudo-ref.
func v0Advertisement(repo *Repo, service string) []byte {
	caps := "report-status side-band-64k delete-refs ofs-delta agent=" + forgeAgent +
		" symref=HEAD:refs/heads/main"
	if service == "git-upload-pack" {
		caps = "multi_ack multi_ack_detailed thin-pack side-band side-band-64k ofs-delta shallow no-progress agent=" +
			forgeAgent + " symref=HEAD:refs/heads/main"
	}
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlush())
	lookup := repo.LookupObject
	if len(repo.Refs) == 0 {
		buf.Write(pktLine([]byte(ZeroOID + " capabilities^{}\x00" + caps + "\n")))
		buf.Write(pktFlush())
		return buf.Bytes()
	}
	headOID := repo.Refs["refs/heads/main"]
	if headOID == "" {
		headOID = ZeroOID
	}
	buf.Write(pktLine([]byte(headOID + " HEAD\x00" + caps + "\n")))
	for _, name := range repo.SortedRefNames() {
		oid := repo.Refs[name]
		buf.Write(pktLine([]byte(oid + " " + name + "\n")))
		if obj := lookup(oid); obj != nil && obj.Type == objTag {
			peeled := PeelToCommit(oid, lookup)
			buf.Write(pktLine([]byte(peeled + " " + name + "^{}\n")))
		}
	}
	buf.Write(pktFlush())
	return buf.Bytes()
}

// -------------------------------------------------------------------------------------------
// info/refs
// -------------------------------------------------------------------------------------------

func (s *Store) handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo, service string) {
	if service != "git-upload-pack" && service != "git-receive-pack" {
		s.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	var body []byte
	if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
		body = v2Advertisement()
	} else {
		body = v0Advertisement(repo, service)
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// -------------------------------------------------------------------------------------------
// v2 ls-refs
// -------------------------------------------------------------------------------------------

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// handleLsRefs answers a v2 ls-refs command.
func (s *Store) handleLsRefs(w http.ResponseWriter, repo *Repo, args lsRefsArgs) {
	var buf bytes.Buffer
	lookup := repo.LookupObject
	matches := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, p := range args.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	headOID, headExists := repo.Refs["refs/heads/main"]
	if matches("HEAD") {
		if headExists {
			line := headOID + " HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/main"
			}
			buf.Write(pktLine([]byte(line + "\n")))
		} else if args.unborn {
			line := "unborn HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/main"
			}
			buf.Write(pktLine([]byte(line + "\n")))
		}
	}
	for _, name := range repo.SortedRefNames() {
		if !matches(name) {
			continue
		}
		oid := repo.Refs[name]
		line := oid + " " + name
		if args.peel {
			if obj := lookup(oid); obj != nil && obj.Type == objTag {
				line += " peeled:" + PeelToCommit(oid, lookup)
			}
		}
		buf.Write(pktLine([]byte(line + "\n")))
	}
	buf.Write(pktFlush())
	body := buf.Bytes()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// -------------------------------------------------------------------------------------------
// fetch: the send set
// -------------------------------------------------------------------------------------------

// closureOf returns every object reachable from roots, silently skipping
// roots the store does not hold.
func closureOf(roots []string, lookup func(string) *Object) map[string]bool {
	out := map[string]bool{}
	for _, root := range roots {
		seen, _ := CollectReachable([]string{root}, lookup)
		for oid := range seen {
			out[oid] = true
		}
	}
	return out
}

// treeClosure returns a tree object and everything below it (trees, blobs).
func treeClosure(treeOID string, lookup func(string) *Object) map[string]bool {
	out := map[string]bool{}
	queue := []string{treeOID}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		out[oid] = true
		if obj.Type != objTree {
			continue
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Mode != "160000" {
				queue = append(queue, e.OID)
			}
		}
	}
	return out
}

// fetchSendSet computes the objects a fetch must deliver: everything
// reachable from wants within depth generations, minus what the client
// already holds (the haves' closure). depth 0 means unlimited. It also
// returns the new shallow boundary and the boundary that stopped being
// shallow (deepen), both sorted.
func (r *Repo) fetchSendSet(wants, haves []string, depth int, clientShallow []string) (map[string]*Object, []string, []string, error) {
	lookup := r.LookupObject
	for _, want := range wants {
		if lookup(want) == nil {
			return nil, nil, nil, fmt.Errorf("want %s is not in the repository", want)
		}
	}
	// What the client already holds. A have that is one of the client's
	// shallow boundaries holds ONLY itself and its tree: its ancestry was
	// cut off by the shallow clone, so the ancestors are NOT held (a plain
	// reachability closure of the have would wrongly exclude them from the
	// send set — that is what broke deepen). Any other have is a tip of a
	// complete history, so its full closure is held.
	held := map[string]bool{}
	shallowSet := map[string]bool{}
	for _, s := range clientShallow {
		shallowSet[s] = true
	}
	for _, h := range haves {
		obj := lookup(h)
		if obj == nil {
			continue
		}
		if shallowSet[h] && obj.Type == objCommit {
			held[h] = true
			if tree, _, err := ParseCommit(obj.Data); err == nil {
				for sub := range treeClosure(tree, lookup) {
					held[sub] = true
				}
			}
		} else if shallowSet[h] && obj.Type == objTree {
			for sub := range treeClosure(h, lookup) {
				held[sub] = true
			}
		} else {
			for oid := range closureOf([]string{h}, lookup) {
				held[oid] = true
			}
		}
	}

	// BFS over commits from the wants; dist counts parent edges from a want.
	dist := map[string]int{}
	includedCommits := map[string]bool{}
	extra := map[string]bool{} // non-commit wants: tag/tree/blob objects
	type item struct {
		oid string
		d   int
	}
	queue := []item{}
	var pushWant func(oid string, d int)
	pushWant = func(oid string, d int) {
		obj := lookup(oid)
		if obj == nil {
			return
		}
		switch obj.Type {
		case objCommit:
			queue = append(queue, item{oid, d})
		case objTag:
			extra[oid] = true
			target, _, _, err := ParseTag(obj.Data)
			if err == nil {
				pushWant(target, d)
			}
		default:
			extra[oid] = true
		}
	}
	for _, want := range wants {
		pushWant(want, 0)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if includedCommits[cur.oid] {
			continue
		}
		if depth > 0 && cur.d >= depth {
			continue // cut off: a parent beyond the depth limit
		}
		includedCommits[cur.oid] = true
		dist[cur.oid] = cur.d
		obj := lookup(cur.oid)
		if obj == nil || obj.Type != objCommit {
			continue
		}
		_, parents, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range parents {
			if !includedCommits[p] {
				queue = append(queue, item{p, cur.d + 1})
			}
		}
	}

	included := map[string]bool{}
	for oid := range extra {
		included[oid] = true
		if obj := lookup(oid); obj != nil && obj.Type == objTree {
			for sub := range treeClosure(oid, lookup) {
				included[sub] = true
			}
		}
	}
	for oid := range includedCommits {
		included[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		tree, _, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for sub := range treeClosure(tree, lookup) {
			included[sub] = true
		}
	}

	send := map[string]*Object{}
	for oid := range included {
		if held[oid] {
			continue
		}
		if obj := lookup(oid); obj != nil {
			send[oid] = obj
		}
	}

	var shallow, unshallow []string
	if depth > 0 {
		for oid, d := range dist {
			if d != depth-1 || held[oid] {
				continue
			}
			obj := lookup(oid)
			if obj == nil || obj.Type != objCommit {
				continue
			}
			_, parents, err := ParseCommit(obj.Data)
			if err != nil || len(parents) == 0 {
				continue
			}
			shallow = append(shallow, oid)
		}
		for _, boundary := range clientShallow {
			if !includedCommits[boundary] {
				continue
			}
			obj := lookup(boundary)
			if obj == nil || obj.Type != objCommit {
				continue
			}
			_, parents, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			complete := true
			for _, p := range parents {
				if !includedCommits[p] {
					complete = false
					break
				}
			}
			if complete {
				unshallow = append(unshallow, boundary)
			}
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return send, shallow, unshallow, nil
}

// writeSidebandPack appends a pack to buf as sideband band-1 packets.
func writeSidebandPack(buf *bytes.Buffer, pack []byte) {
	const chunk = 32768
	for off := 0; off < len(pack); off += chunk {
		end := off + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 0, 1+(end-off))
		payload = append(payload, 1)
		payload = append(payload, pack[off:end]...)
		buf.Write(pktLine(payload))
	}
}

// fetchV2Response writes a protocol v2 fetch response for the computed set.
// The acknowledgments section is sent only when the client sent haves:
// with no haves there is nothing to acknowledge, and real git expects the
// packfile section straight away (protocol-v2: the acknowledgment section
// is omitted when the request carries no 'have' lines). When haves are
// present the server ACKs the ones it holds and sends `ready`: a NAK with
// no `ready` tells real git that no further section may follow, which would
// forbid the shallow-info section a deepen needs. Sections are separated by
// delim (0001), as real git's upload-pack does; the packfile section ends
// with a flush.
func (s *Store) fetchV2Response(w http.ResponseWriter, send map[string]*Object, shallow, unshallow []string, depth int, withAck bool, acks []string) {
	var buf bytes.Buffer
	if withAck {
		buf.Write(pktLine([]byte("acknowledgments\n")))
		if len(acks) == 0 {
			buf.Write(pktLine([]byte("NAK\n")))
		} else {
			for _, oid := range acks {
				buf.Write(pktLine([]byte("ACK " + oid + "\n")))
			}
			buf.Write(pktLine([]byte("ready\n")))
		}
		buf.Write(pktDelim())
	}
	if depth > 0 {
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, oid := range shallow {
			buf.Write(pktLine([]byte("shallow " + oid + "\n")))
		}
		for _, oid := range unshallow {
			buf.Write(pktLine([]byte("unshallow " + oid + "\n")))
		}
		buf.Write(pktDelim())
	}
	buf.Write(pktLine([]byte("packfile\n")))
	writeSidebandPack(&buf, WritePack(send))
	buf.Write(pktFlush())
	body := buf.Bytes()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// fetchV2Error writes a v2 fetch response carrying a sideband band-3 error.
func (s *Store) fetchV2Error(w http.ResponseWriter, msg string) {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("acknowledgments\n")))
	buf.Write(pktLine([]byte("NAK\n")))
	buf.Write(pktDelim())
	buf.Write(pktLine([]byte("packfile\n")))
	payload := append([]byte{3}, []byte(msg+"\n")...)
	buf.Write(pktLine(payload))
	buf.Write(pktFlush())
	body := buf.Bytes()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// fetchV0Response writes a protocol v0 fetch response: NAK, shallow lines,
// then the pack as sideband packets.
func (s *Store) fetchV0Response(w http.ResponseWriter, send map[string]*Object, shallow, unshallow []string) {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("NAK\n")))
	for _, oid := range shallow {
		buf.Write(pktLine([]byte("shallow " + oid + "\n")))
	}
	for _, oid := range unshallow {
		buf.Write(pktLine([]byte("unshallow " + oid + "\n")))
	}
	writeSidebandPack(&buf, WritePack(send))
	buf.Write(pktFlush())
	body := buf.Bytes()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// -------------------------------------------------------------------------------------------
// upload-pack
// -------------------------------------------------------------------------------------------

// parseV2Request splits a v2 request body into its command and its arguments.
// A body that ends without a trailing flush is accepted: the request is
// complete once the argument section is over.
func parseV2Request(body []byte) (command string, args []string, err error) {
	rd := &pktReader{b: body}
	inArgs := false
	for {
		payload, kind, err := rd.next()
		if err != nil {
			if err == io.ErrUnexpectedEOF {
				return command, args, nil
			}
			return "", nil, err
		}
		switch kind {
		case pktFlushKind:
			return command, args, nil
		case pktDelimKind:
			inArgs = true
		case pktData:
			line := strings.TrimSuffix(string(payload), "\n")
			if strings.HasPrefix(line, "command=") {
				command = strings.TrimPrefix(line, "command=")
			} else if inArgs {
				args = append(args, line)
			}
			// agent= / object-format= lines before the delim are ignored.
		}
	}
}

func (s *Store) handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	if r.Header.Get("Git-Protocol") == "version=2" {
		command, args, err := parseV2Request(body)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "malformed request")
			return
		}
		switch command {
		case "ls-refs":
			parsed := lsRefsArgs{}
			for _, arg := range args {
				switch {
				case arg == "peel":
					parsed.peel = true
				case arg == "symrefs":
					parsed.symrefs = true
				case arg == "unborn":
					parsed.unborn = true
				case strings.HasPrefix(arg, "ref-prefix "):
					parsed.prefixes = append(parsed.prefixes, strings.TrimPrefix(arg, "ref-prefix "))
				}
			}
			s.handleLsRefs(w, repo, parsed)
			return
		case "fetch":
			var wants, haves, shallow []string
			depth := 0
			for _, arg := range args {
				switch {
				case strings.HasPrefix(arg, "want "):
					wants = append(wants, strings.TrimPrefix(arg, "want "))
				case strings.HasPrefix(arg, "have "):
					haves = append(haves, strings.TrimPrefix(arg, "have "))
				case strings.HasPrefix(arg, "shallow "):
					shallow = append(shallow, strings.TrimPrefix(arg, "shallow "))
				case strings.HasPrefix(arg, "deepen "):
					fmt.Sscanf(strings.TrimPrefix(arg, "deepen "), "%d", &depth)
				}
			}
			send, newShallow, unshallow, err := repo.fetchSendSet(wants, haves, depth, shallow)
			if err != nil {
				s.fetchV2Error(w, err.Error())
				return
			}
			var acks []string
			for _, have := range haves {
				if repo.LookupObject(have) != nil {
					acks = append(acks, have)
				}
			}
			s.fetchV2Response(w, send, newShallow, unshallow, depth, len(haves) > 0, acks)
			return
		default:
			s.writeError(w, http.StatusBadRequest, "unknown command")
			return
		}
	}

	// Protocol v0 fetch: wants (with shallow/deepen), flush, haves, done, flush.
	rd := &pktReader{b: body}
	var wants, haves, shallow []string
	depth := 0
	section := 0
	sectionsDone := false
	for !sectionsDone {
		payload, kind, err := rd.next()
		if err != nil {
			// Real git ends the v0 request after `done` with no trailing
			// flush: EOF past the first section is a complete request.
			if err == io.ErrUnexpectedEOF && section >= 1 {
				break
			}
			s.writeError(w, http.StatusBadRequest, "malformed request")
			return
		}
		switch kind {
		case pktFlushKind:
			section++
			if section >= 2 {
				sectionsDone = true
			}
		case pktData:
			line := strings.TrimSuffix(string(payload), "\n")
			if section == 0 {
				switch {
				case strings.HasPrefix(line, "want "):
					rest := strings.TrimPrefix(line, "want ")
					if sp := strings.IndexByte(rest, ' '); sp >= 0 {
						rest = rest[:sp]
					}
					wants = append(wants, rest)
				case strings.HasPrefix(line, "shallow "):
					shallow = append(shallow, strings.TrimPrefix(line, "shallow "))
				case strings.HasPrefix(line, "deepen "):
					fmt.Sscanf(strings.TrimPrefix(line, "deepen "), "%d", &depth)
				}
			} else {
				if strings.HasPrefix(line, "have ") {
					haves = append(haves, strings.TrimPrefix(line, "have "))
				}
			}
		}
	}
	send, newShallow, unshallow, err := repo.fetchSendSet(wants, haves, depth, shallow)
	if err != nil {
		// v0 has no band-3 channel the client tolerates here; answer NAK with
		// an empty pack so the exchange stays framed.
		s.fetchV0Response(w, map[string]*Object{}, nil, nil)
		return
	}
	s.fetchV0Response(w, send, newShallow, unshallow)
}

// -------------------------------------------------------------------------------------------
// receive-pack
// -------------------------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// handleReceivePack answers a v0 receive-pack push: unpack the pack, store
// its objects, apply each ref update (fast-forward only) and report per ref.
func (s *Store) handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	rd := &pktReader{b: body}
	var commands []pushCommand
	var caps string
	for {
		payload, kind, err := rd.next()
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "malformed request")
			return
		}
		if kind == pktFlushKind {
			break
		}
		if kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if i := strings.IndexByte(line, 0); i >= 0 {
			caps = line[i+1:]
			line = line[:i]
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || !IsOID(parts[0]) || !IsOID(parts[1]) || !strings.HasPrefix(parts[2], "refs/") {
			s.writeError(w, http.StatusBadRequest, "malformed request")
			return
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	if len(commands) == 0 {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	pack := rd.rest()

	sideband := strings.Contains(" "+caps+" ", " side-band-64k ")

	// Unpack: every update needs a pack, even an empty one.
	unpackOK := true
	unpackDetail := "ok"
	var unpacked map[string]*Object
	needsPack := false
	for _, cmd := range commands {
		if cmd.newOID != ZeroOID {
			needsPack = true
		}
	}
	if needsPack && len(pack) == 0 {
		unpackOK = false
		unpackDetail = "unpacker error"
	} else if len(pack) > 0 {
		objects, err := ReadPack(pack, repo.LookupObject)
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			unpacked = objects
		}
	}

	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]refResult, 0, len(commands))

	s.mu.Lock()
	if unpackOK && unpacked != nil {
		for oid, obj := range unpacked {
			repo.Objects[oid] = obj
		}
	}
	lookup := repo.LookupObject
	for _, cmd := range commands {
		fail := func(reason string) { results = append(results, refResult{cmd.ref, false, reason}) }
		succeed := func() { results = append(results, refResult{cmd.ref, true, ""}) }
		if !unpackOK {
			fail("unpack failed")
			continue
		}
		cur, exists := repo.Refs[cmd.ref]
		switch {
		case cmd.newOID == ZeroOID: // delete
			if !exists {
				fail("remote ref does not exist")
			} else if cmd.oldOID != cur {
				fail("old value does not match")
			} else {
				delete(repo.Refs, cmd.ref)
				succeed()
			}
		case cmd.oldOID == ZeroOID: // create
			if exists {
				fail("already exists")
			} else if lookup(cmd.newOID) == nil {
				fail("object not found")
			} else {
				repo.Refs[cmd.ref] = cmd.newOID
				succeed()
			}
		default: // update
			if !exists {
				fail("remote ref does not exist")
			} else if cmd.oldOID != cur {
				fail("old value does not match")
			} else if cmd.newOID == cur {
				succeed()
			} else if lookup(cmd.newOID) == nil {
				fail("object not found")
			} else if !isFastForward(cur, cmd.newOID, lookup) {
				fail("non-fast-forward")
			} else {
				repo.Refs[cmd.ref] = cmd.newOID
				succeed()
			}
		}
	}
	s.mu.Unlock()

	// report-status stream, pkt-lined, optionally wrapped in sideband band 1.
	var report bytes.Buffer
	if unpackOK {
		report.Write(pktLine([]byte("unpack ok\n")))
	} else {
		report.Write(pktLine([]byte("unpack " + unpackDetail + "\n")))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktLine([]byte("ok " + res.ref + "\n")))
		} else {
			report.Write(pktLine([]byte("ng " + res.ref + " " + res.reason + "\n")))
		}
	}
	report.Write(pktFlush())

	var bodyOut bytes.Buffer
	if sideband {
		stream := report.Bytes()
		const chunk = 60000
		for off := 0; off < len(stream); off += chunk {
			end := off + chunk
			if end > len(stream) {
				end = len(stream)
			}
			payload := make([]byte, 0, 1+(end-off))
			payload = append(payload, 1)
			payload = append(payload, stream[off:end]...)
			bodyOut.Write(pktLine(payload))
		}
		bodyOut.Write(pktFlush())
	} else {
		bodyOut.Write(report.Bytes())
	}
	out := bodyOut.Bytes()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("X-GitHub-Request-Id", s.RequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// isFastForward reports whether newOID descends from oldOID (both commits),
// or the two are equal. Non-commit objects never fast-forward.
func isFastForward(oldOID, newOID string, lookup func(string) *Object) bool {
	oldObj := lookup(oldOID)
	newObj := lookup(newOID)
	if oldObj == nil || newObj == nil {
		return false
	}
	if oldObj.Type != objCommit || newObj.Type != objCommit {
		return false
	}
	return IsDescendant(newOID, oldOID, lookup)
}
