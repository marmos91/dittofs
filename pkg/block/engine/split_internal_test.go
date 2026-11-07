package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// splitStore answers an existence commit as its rules say: a file in inMap
// fails in the map, the others committing; a file in whole fails the whole
// call; marks beside a file fail it with marksErr when set. It records each
// call's files by name, and a "*" for its marks.
type splitStore struct {
	Existence
	names    map[entity.FileID]string
	inMap    map[string]bool
	whole    map[string]bool
	err      error // what a whole-call failure answers
	marksErr error
	calls    []string
}

// errConflict is a conflict the store kept retrying until the call's bound.
var errConflict = errors.Join(kvstore.ErrConflict, context.DeadlineExceeded)

func (s *splitStore) Commit(_ context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	var call []string
	failWhole := false
	for _, pe := range p {
		n := s.names[pe.File]
		call = append(call, n)
		failWhole = failWhole || s.whole[n]
	}
	if marks != nil {
		call = append(call, "*")
		if s.marksErr != nil && len(p) > 0 {
			s.calls = append(s.calls, strings.Join(call, ""))
			return nil, s.marksErr
		}
	}
	s.calls = append(s.calls, strings.Join(call, ""))
	if failWhole {
		return nil, s.err
	}
	m := map[entity.FileID]error{}
	for _, pe := range p {
		if s.inMap[s.names[pe.File]] {
			m[pe.File] = errors.New("fails alone")
		}
	}
	return m, nil
}

// Every failure shape of an existence commit against the calls the split
// makes and the files it reports failed: no committed file is sent again, and
// no failing file is sent again beside one that committed.
func TestTheSplit(t *testing.T) {
	for _, c := range []struct {
		name         string
		inMap, whole string
		calls        []string
		failed       string
	}{
		{"none fails", "", "", []string{"abcd"}, ""},
		{"a file in the map", "c", "", []string{"abcd"}, "c"},
		{"a whole-call error", "", "d", []string{"abcd", "ab", "cd", "c", "d"}, "d"},
		{"two failing files", "", "bd", []string{"abcd", "ab", "a", "b", "cd", "c", "d"}, "bd"},
		{"every file failing", "", "abcd", []string{"abcd", "ab", "a", "b", "cd", "c", "d"}, "abcd"},
		{"one in the map, one failing the call", "a", "d", []string{"abcd", "ab", "cd", "c", "d"}, "ad"},
		{"a whole-call error that is no conflict", "", "d", []string{"abcd", "ab", "cd", "c", "d"}, "d"},
		{"the store closed", "", "d", []string{"abcd"}, "abcd"},
		{"a store that answers nothing", "", "d", []string{"abcd"}, "abcd"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &splitStore{names: map[entity.FileID]string{}, inMap: set(c.inMap), whole: set(c.whole), err: errConflict}
			switch {
			case strings.Contains(c.name, "no conflict"):
				s.err = errors.New("a file's record does not decode")
			case strings.Contains(c.name, "closed"):
				s.err = kvstore.ErrClosed
			case strings.Contains(c.name, "answers nothing"):
				s.err = context.DeadlineExceeded
			}
			var pending []kvstore.PendingExistence
			byName := map[string]entity.FileID{}
			for _, n := range "abcd" {
				f := entity.NewFileID()
				s.names[f], byName[string(n)] = string(n), f
				pending = append(pending, kvstore.PendingExistence{File: f})
			}
			e := &Engine{d: Deps{Existence: s}}
			failed, alone := map[entity.FileID]error{}, map[entity.FileID]bool{}
			_, _ = e.commitSplit(context.Background(), pending, nil, failed, alone)
			if !slices.Equal(s.calls, c.calls) {
				t.Errorf("calls %v, want %v", s.calls, c.calls)
			}
			var got []string
			for f := range failed {
				got = append(got, s.names[f])
			}
			slices.Sort(got)
			if strings.Join(got, "") != c.failed {
				t.Errorf("failed %v, want %q", got, c.failed)
			}
		})
	}
}

// Every failure shape on a batch carrying the marks: one call at a time
// carries them, through a split on its first half, until they commit; beside
// a file failing alone they go in a call of their own; a marks error has the
// batch retried at once without them and splits nothing.
func TestTheSplitCarriesTheMarks(t *testing.T) {
	for _, c := range []struct {
		name         string
		inMap, whole string
		calls        []string
		failed       string
		recorded     bool
	}{
		{"none fails", "", "", []string{"abcd*"}, "", true},
		{"a file in the map", "c", "", []string{"abcd*"}, "c", true},
		{"a whole-call error", "", "d", []string{"abcd*", "ab*", "cd", "c", "d"}, "d", true},
		{"two failing files", "", "bd", []string{"abcd*", "ab*", "a*", "b", "cd", "c", "d"}, "bd", true},
		{"every file failing", "", "abcd", []string{"abcd*", "ab*", "a*", "*", "b", "cd", "c", "d"}, "abcd", true},
		{"the store closed", "", "d", []string{"abcd*"}, "abcd", false},
		{"marks not written", "", "", []string{"abcd*", "abcd"}, "", false},
		{"the open generation moved", "c", "", []string{"abcd*", "abcd"}, "c", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &splitStore{names: map[entity.FileID]string{}, inMap: set(c.inMap), whole: set(c.whole), err: errConflict}
			switch c.name {
			case "the store closed":
				s.err = kvstore.ErrClosed
			case "marks not written":
				s.marksErr = kvstore.ErrSyncMarks
			case "the open generation moved":
				s.marksErr = kvstore.ErrOpenGenMoved
			}
			var pending []kvstore.PendingExistence
			for _, n := range "abcd" {
				f := entity.NewFileID()
				s.names[f] = string(n)
				pending = append(pending, kvstore.PendingExistence{File: f})
			}
			e := &Engine{d: Deps{Existence: s}}
			failed, alone := map[entity.FileID]error{}, map[entity.FileID]bool{}
			recorded, err := e.commitSplit(context.Background(), pending, &kvstore.MarksWrite{}, failed, alone)
			if !slices.Equal(s.calls, c.calls) {
				t.Errorf("calls %v, want %v", s.calls, c.calls)
			}
			var got []string
			for f := range failed {
				got = append(got, s.names[f])
			}
			slices.Sort(got)
			if strings.Join(got, "") != c.failed {
				t.Errorf("failed %v, want %q", got, c.failed)
			}
			if recorded != c.recorded || (s.marksErr != nil && !errors.Is(err, s.marksErr)) {
				t.Errorf("recorded %v, err %v; want recorded %v", recorded, err, c.recorded)
			}
		})
	}
}

// One file of 64 failing the whole call costs the others at most log₂ 64
// halvings, two calls each.
func TestTheSplitIsBounded(t *testing.T) {
	s := &splitStore{names: map[entity.FileID]string{}, whole: map[string]bool{"x": true}, err: errConflict}
	var pending []kvstore.PendingExistence
	for i := range 64 {
		f := entity.NewFileID()
		s.names[f] = "."
		if i == 37 {
			s.names[f] = "x"
		}
		pending = append(pending, kvstore.PendingExistence{File: f})
	}
	e := &Engine{d: Deps{Existence: s}}
	_, _ = e.commitSplit(context.Background(), pending, nil, map[entity.FileID]error{}, map[entity.FileID]bool{})
	if n := len(s.calls); n > 1+2*6 {
		t.Errorf("%d calls for one failing file of 64, want at most 13", n)
	}
	if n := e.splits.Load(); n != 6 {
		t.Errorf("engine_group_commit_splits_total %d, want 6", n)
	}
}

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, c := range s {
		m[string(c)] = true
	}
	return m
}
