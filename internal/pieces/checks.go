package pieces

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// PartialTag marks the snapshot of a batch: not a backup, only the parent of the next batch.
// Each run forgets its own; maintenance forgets leftovers older than a day.
const PartialTag = "kind=guest-pieces-partial"

// CheckError is a run that a check rejected; Check is C1, C2, C3 or accept.
type CheckError struct {
	Check  string
	Detail string
}

func (e *CheckError) Error() string { return fmt.Sprintf("check %s failed: %s", e.Check, e.Detail) }

func checkFail(check, format string, a ...any) error {
	return &CheckError{Check: check, Detail: fmt.Sprintf(format, a...)}
}

// isExtra reports whether rel is a file the final batch writes besides the pieces.
func isExtra(rel string) bool {
	return rel == ManifestName || strings.HasPrefix(rel, ConfigDir+"/") && !strings.Contains(rel[len(ConfigDir)+1:], "/")
}

// CheckC1 compares restic's action for every file with what the batch did: each piece renamed
// in for the batch is new or modified, the manifest and config files are whatever restic
// found, every other file of the tree is unchanged, and restic reported exactly the files of
// the tree, each once. A placeholder restic read (forced, wrong parent, touched mtime, blobs
// missing), a changed piece whose mtime stayed the parent's and a missing placeholder all
// fail here.
func CheckC1(rep *BackupReport, b *Batch) error {
	if len(rep.Outside) > 0 {
		return checkFail("C1", "restic stored %s, which is outside the tree", rep.Outside[0])
	}
	if len(rep.Twice) > 0 {
		return checkFail("C1", "restic reported %s twice", rep.Twice[0])
	}
	renamed := make(map[string]bool, len(b.Pieces))
	for _, p := range b.Pieces {
		renamed[p.Rel] = true
	}
	extra := map[string]bool{}
	for _, e := range b.Extra {
		extra[e] = true
	}
	for rel, a := range rep.Actions {
		if !b.InTree(rel) {
			return checkFail("C1", "restic stored %s, which the run did not write", rel)
		}
		switch {
		case renamed[rel]:
			if a != "new" && a != "modified" {
				return checkFail("C1", "%s was written by this batch but restic found it %s", rel, a)
			}
		case extra[rel]:
			if a != "new" && a != "modified" && a != "unchanged" {
				return checkFail("C1", "%s: restic action %s", rel, a)
			}
		default:
			if a != "unchanged" {
				return checkFail("C1", "%s is a placeholder but restic found it %s (it read the placeholder)", rel, a)
			}
		}
	}
	if len(rep.Actions) != b.TreeFiles {
		for _, p := range b.Pieces {
			if _, ok := rep.Actions[p.Rel]; !ok {
				return checkFail("C1", "restic did not report %s", p.Rel)
			}
		}
		return checkFail("C1", "restic reported %d files, the tree has %d (a placeholder is missing)", len(rep.Actions), b.TreeFiles)
	}
	return nil
}

// CheckC2 checks restic's exit code, its error lines and its counts: new plus changed files
// equal the pieces renamed in (plus the manifest and config files restic found new or
// modified), and unmodified files are all the others.
func CheckC2(rep *BackupReport, b *Batch) error {
	switch {
	case rep.Exit != 0:
		return checkFail("C2", "restic backup ended with exit %d: %s", rep.Exit, strings.Join(rep.Messages, "; "))
	case len(rep.Messages) > 0:
		return checkFail("C2", "restic backup reported an error: %s", rep.Messages[0])
	case rep.Summary == nil || !isHex(rep.Summary.SnapshotID, 64):
		return checkFail("C2", "restic backup gave no summary with a snapshot")
	}
	written := len(b.Pieces)
	for _, e := range b.Extra {
		if a := rep.Actions[e]; a == "new" || a == "modified" {
			written++
		}
	}
	s := rep.Summary
	if s.FilesNew+s.FilesChanged != written || s.FilesUnmodified != b.TreeFiles-written {
		return checkFail("C2", "restic counted %d new, %d changed, %d unmodified files; the batch wrote %d of %d", s.FilesNew, s.FilesChanged, s.FilesUnmodified, written, b.TreeFiles)
	}
	return nil
}

// CheckC3 checks the tree diff of the run's original parent against the final snapshot: a
// piece with changed content must be one the run kept; pieces and their folders may be added
// or removed only on a disk whose size changed; the manifest and the config files may change;
// nothing else.
func CheckC3(changes []DiffChange, kept map[string]bool, resized map[string]bool) error {
	for _, c := range changes {
		rel := strings.TrimSuffix(c.Rel, "/")
		dir := strings.HasSuffix(c.Rel, "/")
		if isExtra(rel) || rel == ConfigDir && dir {
			continue
		}
		dev, _, isPiece := parsePiece(rel)
		if !isPiece && dir {
			if parts := strings.Split(rel, "/"); parts[0] == DisksDir && len(parts) >= 2 && len(parts) <= 3 {
				dev, isPiece = parts[1], true
			}
		}
		if !isPiece {
			return checkFail("C3", "%s changed (%s); only pieces, the manifest and the config may", c.Rel, c.Modifier)
		}
		switch {
		case strings.ContainsAny(c.Modifier, "+-"):
			if !resized[dev] {
				return checkFail("C3", "%s %s on %s, whose size did not change", c.Rel, map[bool]string{true: "added", false: "removed"}[strings.Contains(c.Modifier, "+")], dev)
			}
		case strings.Contains(c.Modifier, "T"):
			return checkFail("C3", "%s changed its type", c.Rel)
		case strings.Contains(c.Modifier, "M") && !dir:
			if !kept[rel] {
				return checkFail("C3", "%s has new content although the run did not write it", rel)
			}
		}
	}
	return nil
}

// Store runs the batches of one run with restic, checks each (C1, C2) and accepts the final
// snapshot (C3, the guest tags, the tree read), or forgets every snapshot of the run.
type Store struct {
	R           *Restic
	Root, Host  string
	GuestTags   []string // the tags of an accepted snapshot: today's guest tags
	PartialTags []string // PartialTag, the unit and the vmid tag
	Original    string   // the run's parent, "" for a run without one

	prev     string   // the parent of the next batch
	batches  []string // this run's batch snapshots, in order
	last     *Batch
	accepted string
	runs     int

	// ForgetError is set when the partial snapshots of an accepted run could not be forgotten.
	ForgetError error

	// tests only
	force      bool
	skipChecks bool
	after      func()
}

// Runs returns how many batches restic ran in this run.
func (s *Store) Runs() int { return s.runs }

// Batches returns the batch snapshots of this run so far.
func (s *Store) Batches() []string { return append([]string(nil), s.batches...) }

var errSkipped = errors.New("pieces: checks skipped (test)")

// RunBatch is the writer's batch function: restic backup with the partial tags and the
// previous batch (or the original parent) as parent, then C1 and C2.
func (s *Store) RunBatch(ctx context.Context, b *Batch) error {
	parent := s.prev
	if len(s.batches) == 0 && parent == "" {
		parent = s.Original
	}
	s.runs++
	rep, err := s.R.Backup(ctx, BackupArgs{Root: s.Root, Host: s.Host, Tags: s.PartialTags, Parent: parent, Force: s.force})
	if rep != nil && rep.Summary != nil && isHex(rep.Summary.SnapshotID, 64) {
		s.batches = append(s.batches, rep.Summary.SnapshotID)
	}
	if err != nil {
		return err
	}
	if s.skipChecks {
		if s.after != nil {
			s.after()
		}
		return errSkipped
	}
	// a restic that failed is named by its exit code and errors (C2) before C1 compares the
	// files it did or did not report
	if rep.Exit != 0 || len(rep.Messages) > 0 {
		return CheckC2(rep, b)
	}
	if err := CheckC1(rep, b); err != nil {
		return err
	}
	if err := CheckC2(rep, b); err != nil {
		return err
	}
	s.prev = rep.Summary.SnapshotID
	s.last = b
	return nil
}

// Accept checks the run's final snapshot and makes it the backup: C3 against the original
// parent (when there is one), `tag --set` with the guest tags, then a read of the new
// snapshot's tree, which must hold exactly the final batch's files. Then the partial
// snapshots of the run are forgotten; a failure to forget leaves them to the next run and to
// maintenance and does not undo the acceptance (ForgetError says so).
func (s *Store) Accept(ctx context.Context, kept []Piece, resized map[string]bool) (string, error) {
	if s.last == nil || !s.last.Final || len(s.batches) == 0 || s.prev != s.batches[len(s.batches)-1] {
		return "", checkFail("accept", "the run has no checked final batch")
	}
	final := s.prev
	if s.Original != "" {
		changes, err := s.R.Diff(ctx, s.Original, final, s.Root)
		if err != nil {
			return "", checkFail("C3", "%v", err)
		}
		k := make(map[string]bool, len(kept))
		for _, p := range kept {
			k[p.Rel] = true
		}
		if err := CheckC3(changes, k, resized); err != nil {
			return "", err
		}
	}
	id, err := s.R.TagSet(ctx, final, s.GuestTags)
	if err != nil {
		return "", checkFail("accept", "%v", err)
	}
	s.batches[len(s.batches)-1] = id // the tagged snapshot replaced the final one
	l, err := List(ctx, s.R, id, s.Root)
	if err != nil {
		return "", checkFail("accept", "read of the new snapshot: %v", err)
	}
	files := make([]string, 0, len(l.files))
	for rel := range l.files {
		files = append(files, rel)
	}
	sort.Strings(files)
	for _, rel := range files {
		if !s.last.InTree(rel) {
			return "", checkFail("accept", "the new snapshot holds %s, which the run did not write", rel)
		}
	}
	if len(files) != s.last.TreeFiles {
		return "", checkFail("accept", "the new snapshot holds %d files, the run wrote %d", len(files), s.last.TreeFiles)
	}
	partials := s.batches[:len(s.batches)-1]
	s.batches, s.accepted = []string{id}, id
	if err := s.R.Forget(ctx, partials...); err != nil {
		s.ForgetError = err
	}
	return id, nil
}

// Discard forgets every snapshot of this run that is not accepted: the batches and a final
// snapshot that failed its checks. It is called on every rejection and failure.
func (s *Store) Discard(ctx context.Context) error {
	var ids []string
	for _, id := range s.batches {
		if id != s.accepted {
			ids = append(ids, id)
		}
	}
	if err := s.R.Forget(ctx, ids...); err != nil {
		return err
	}
	s.batches = nil
	if s.accepted != "" {
		s.batches = []string{s.accepted}
	}
	return nil
}
