package entity

import (
	"io/fs"
	"testing"
	"time"
)

func regular() File {
	return File{ID: NewFileID(), Share: NewShareID(), Type: Regular, Mode: 0o644, Number: 1}
}

func TestFileValidate(t *testing.T) {
	dir := regular()
	dir.Type, dir.Parent = Directory, NewFileID()
	link := regular()
	link.Type, link.Target = Symlink, []byte("x")
	stream := regular()
	stream.Type, stream.StreamOf = Stream, NewFileID()
	for _, f := range []File{regular(), dir, link, stream} {
		if err := f.Validate(); err != nil {
			t.Errorf("%v: %v", f.Type, err)
		}
	}
	for name, mut := range map[string]func(*File){
		"no ID":                  func(f *File) { f.ID = FileID{} },
		"no number":              func(f *File) { f.Number = 0 },
		"type bits in mode":      func(f *File) { f.Mode |= 0o100000 },
		"target on a regular":    func(f *File) { f.Target = []byte("x") },
		"device on a regular":    func(f *File) { f.Device = DeviceID{Major: 1} },
		"parent on a regular":    func(f *File) { f.Parent = NewFileID() },
		"stream-of on regular":   func(f *File) { f.StreamOf = NewFileID() },
		"symlink with no target": func(f *File) { f.Type = Symlink },
		"directory, no parent":   func(f *File) { f.Type = Directory },
		"unknown type":           func(f *File) { f.Type = 99 },
	} {
		f := regular()
		mut(&f)
		if f.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApply(t *testing.T) {
	alice, bob := NewUser(), NewUser()
	f := regular()
	f.Owner, f.Mode, f.CreateVerifier = alice, 0o6755, []byte("12345678")

	// Every SetAttrs clears the verifier, even one changing nothing else.
	if got := f.Apply(Attrs{}); got.CreateVerifier != nil || got.Mode != 0o6755 {
		t.Errorf("empty SetAttrs: mode %o, verifier %v", got.Mode, got.CreateVerifier)
	}
	// A chown clears setuid, and setgid with group execute.
	if got := f.Apply(Attrs{}.WithOwner(bob)); got.Mode != 0o755 || got.Owner != bob {
		t.Errorf("chown: mode %o owner %v", got.Mode, got.Owner)
	}
	// Without group execute setgid marks mandatory locking and stays.
	f.Mode = 0o6745
	if got := f.Apply(Attrs{}.WithGroup(NewGroup())); got.Mode != 0o2745 {
		t.Errorf("chgrp: mode %o", got.Mode)
	}
	// A chown to the same owner changes nothing.
	f.Mode = 0o4755
	if got := f.Apply(Attrs{}.WithOwner(alice)); got.Mode != 0o4755 {
		t.Errorf("chown to self: mode %o", got.Mode)
	}
	// A directory keeps its bits.
	d := f
	d.Type = Directory
	if got := d.Apply(Attrs{}.WithOwner(bob)); got.Mode != 0o4755 {
		t.Errorf("directory chown: mode %o", got.Mode)
	}
	now := time.Unix(1_800_000_000, 0)
	a := Attrs{}.WithMode(0o17777).WithModify(now).WithSize(9)
	if !a.Changes(AttrMode|AttrModify|AttrSize) || a.Changes(AttrOwner) {
		t.Errorf("mask %b", a.Mask)
	}
	if got := f.Apply(a); got.Mode != 0o7777 || !got.Modify.Equal(now) || got.Size != 9 {
		t.Errorf("apply: %o %v %d", got.Mode, got.Modify, got.Size)
	}
}

func TestFSMode(t *testing.T) {
	f := regular()
	f.Type, f.Mode, f.Parent = Directory, 0o3755, NewFileID()
	if got := f.FSMode(); got != fs.ModeDir|fs.ModeSetgid|fs.ModeSticky|0o755 {
		t.Errorf("FSMode = %v", got)
	}
	if got := (File{Type: CharDevice, Mode: 0o600}).FSMode(); got != fs.ModeDevice|fs.ModeCharDevice|0o600 {
		t.Errorf("char device = %v", got)
	}
}

func TestRootAndPredicates(t *testing.T) {
	id := NewFileID()
	root := File{ID: id, Type: Directory, Parent: id}
	if !root.IsRoot() || (File{ID: id, Type: Directory, Parent: NewFileID()}).IsRoot() {
		t.Error("IsRoot")
	}
	if !(File{Type: Stream}).HasContent() || (File{Type: Directory}).HasContent() || !(File{Type: FIFO}).IsSpecial() {
		t.Error("type predicates")
	}
	f := File{Flags: FlagImmutable | FlagHidden}
	if !f.IsImmutable() || !f.IsHidden() || f.IsAppendOnly() || f.HasFlag(FlagImmutable|FlagReadOnly) {
		t.Error("flags")
	}
}
