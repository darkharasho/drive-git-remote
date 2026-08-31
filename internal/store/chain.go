package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The remote is an append-only chain of immutable links stored as files in a
// per-repo Drive folder:
//
//	0001-root-3f2a91c4d8be.bundle
//	0002-3f2a91c4d8be-77c0aa12e3f5.bundle
//	0003-77c0aa12e3f5-91bb04de77a1.refs
//
// Each name encodes the sequence number, the tip hash the link was built
// against, and the tip hash it produces. Nothing is ever overwritten, so an
// interrupted push leaves the remote in its previous valid state and two
// racing pushes produce two siblings off the same parent — detectable, and
// recoverable by pulling and re-pushing.

const (
	// RootTip is the parent of the first link in a chain.
	RootTip = "root"
	// ExtBundle carries git objects plus the complete ref set.
	ExtBundle = ".bundle"
	// ExtRefs carries only a ref set, for pushes with no new objects
	// (a deleted branch, or a ref moved onto an existing commit).
	ExtRefs = ".refs"
	// ExtEncrypted is appended when the repo is client-side encrypted.
	ExtEncrypted = ".age"
	// ArchiveFolder holds links superseded by compaction.
	ArchiveFolder = "archive"
	// MetaFile records immutable repo settings.
	MetaFile = "meta.json"
	// LockFile is the advisory push lock.
	LockFile = ".lock"
)

// Meta is written once at init and never mutated.
type Meta struct {
	Version       int    `json:"version"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	// Encryption is "age" or "none".
	Encryption string `json:"encryption"`
	// Recipient is the age public key, recorded so a machine with the wrong
	// key fails with a clear message instead of a decryption error.
	Recipient string `json:"recipient,omitempty"`
}

// Link is one element of the chain.
type Link struct {
	File      File
	Seq       int
	ParentTip string
	Tip       string
	Ext       string // ExtBundle or ExtRefs
	Encrypted bool
}

// IsBundle reports whether the link carries git objects.
func (l Link) IsBundle() bool { return l.Ext == ExtBundle }

// TipHash fingerprints a complete ref set. Two repos with the same tip hash
// have identical refs, which is what lets push and pull compare remote and
// local state without downloading anything.
func TipHash(refs map[string]string) string {
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s %s\n", n, refs[n])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// LinkName builds the filename for a link.
func LinkName(seq int, parentTip, tip, ext string, encrypted bool) string {
	name := fmt.Sprintf("%04d-%s-%s%s", seq, parentTip, tip, ext)
	if encrypted {
		name += ExtEncrypted
	}
	return name
}

// ParseLink parses a link filename. Non-link files yield ok=false.
func ParseLink(f File) (Link, bool) {
	if f.Folder {
		return Link{}, false
	}
	name := f.Name
	l := Link{File: f}
	if strings.HasSuffix(name, ExtEncrypted) {
		l.Encrypted = true
		name = strings.TrimSuffix(name, ExtEncrypted)
	}
	switch {
	case strings.HasSuffix(name, ExtBundle):
		l.Ext = ExtBundle
	case strings.HasSuffix(name, ExtRefs):
		l.Ext = ExtRefs
	default:
		return Link{}, false
	}
	base := strings.TrimSuffix(name, l.Ext)
	parts := strings.Split(base, "-")
	if len(parts) != 3 {
		return Link{}, false
	}
	seq, err := strconv.Atoi(parts[0])
	if err != nil {
		return Link{}, false
	}
	l.Seq, l.ParentTip, l.Tip = seq, parts[1], parts[2]
	return l, true
}

// ForkError reports two or more links claiming the same position in the chain,
// which is what a lost push race looks like from the outside.
type ForkError struct {
	Seq   int
	Names []string
}

func (e *ForkError) Error() string {
	return fmt.Sprintf("remote has diverged: %d links at position %04d (%s)",
		len(e.Names), e.Seq, strings.Join(e.Names, ", "))
}

// BuildChain orders links and validates that each one's parent is its
// predecessor's tip. A chain that fails validation is reported, never
// silently repaired.
func BuildChain(files []File) ([]Link, error) {
	var links []Link
	for _, f := range files {
		if l, ok := ParseLink(f); ok {
			links = append(links, l)
		}
	}
	if len(links) == 0 {
		return nil, nil
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Seq != links[j].Seq {
			return links[i].Seq < links[j].Seq
		}
		return links[i].File.Name < links[j].File.Name
	})

	// Duplicate sequence numbers mean two machines pushed from the same state.
	for i := 1; i < len(links); i++ {
		if links[i].Seq == links[i-1].Seq {
			names := []string{links[i-1].File.Name}
			for j := i; j < len(links) && links[j].Seq == links[i].Seq; j++ {
				names = append(names, links[j].File.Name)
			}
			return nil, &ForkError{Seq: links[i].Seq, Names: names}
		}
	}
	if links[0].ParentTip != RootTip {
		return nil, fmt.Errorf("remote chain is incomplete: first link %s does not start from %q",
			links[0].File.Name, RootTip)
	}
	for i := 1; i < len(links); i++ {
		if links[i].ParentTip != links[i-1].Tip {
			return nil, fmt.Errorf("remote chain is broken: %s expects parent %s but %s produced %s",
				links[i].File.Name, links[i].ParentTip, links[i-1].File.Name, links[i-1].Tip)
		}
	}
	return links, nil
}

// HeadTip returns the tip hash of the chain's last link, or RootTip when empty.
func HeadTip(chain []Link) string {
	if len(chain) == 0 {
		return RootTip
	}
	return chain[len(chain)-1].Tip
}

// NextSeq returns the sequence number a new link should use.
func NextSeq(chain []Link) int {
	if len(chain) == 0 {
		return 1
	}
	return chain[len(chain)-1].Seq + 1
}
