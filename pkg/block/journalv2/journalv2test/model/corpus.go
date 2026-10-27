package model

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// A corpus file is a sequence of steps as text, one step per line: the
// operation, then each field that is not zero, as in
//
//	write s=1 f=3 off=4096 len=12 b=97
//
// Lines starting with # are notes, such as the seed a sequence was drawn from
// and the failure it shows; blank lines are ignored. Fields are s (share), f
// (file), off, len, b (byte) and pick, all decimal.

// formatCorpus returns steps as a corpus file, with each note as a comment
// line before them.
func formatCorpus(steps []Step, notes ...string) []byte {
	var b bytes.Buffer
	for _, n := range notes {
		for line := range strings.SplitSeq(n, "\n") {
			fmt.Fprintf(&b, "# %s\n", line)
		}
	}
	for _, s := range steps {
		b.WriteString(s.Op.String())
		for _, f := range []struct {
			name string
			v    int64
		}{
			{"s", int64(s.Share)}, {"f", int64(s.File)}, {"off", s.Off}, {"len", s.Len},
			{"b", int64(s.Byte)}, {"pick", int64(s.Pick)},
		} {
			if f.v != 0 {
				fmt.Fprintf(&b, " %s=%d", f.name, f.v)
			}
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// parseCorpus reads a corpus file. It refuses any line it cannot read whole,
// rather than reading it as another step.
func parseCorpus(text []byte) ([]Step, error) {
	var steps []Step
	sc := bufio.NewScanner(bytes.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseStep(line)
		if err != nil {
			return nil, fmt.Errorf("model: corpus line %d %q: %w", n, line, err)
		}
		steps = append(steps, s)
	}
	return steps, sc.Err()
}

func parseStep(line string) (Step, error) {
	words := strings.Fields(line)
	var s Step
	for op := OpWrite; op < opEnd; op++ {
		if op.String() == words[0] {
			s.Op = op
		}
	}
	if s.Op == 0 {
		return s, fmt.Errorf("no operation %q", words[0])
	}
	seen := map[string]bool{}
	for _, w := range words[1:] {
		name, value, found := strings.Cut(w, "=")
		if !found {
			return s, fmt.Errorf("field %q has no value", w)
		}
		if seen[name] {
			return s, fmt.Errorf("field %q given twice", name)
		}
		seen[name] = true
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return s, fmt.Errorf("field %q: %w", name, err)
		}
		limit := map[string]int64{"b": 0xff, "pick": 0xff}
		if l, ok := limit[name]; ok && (v < 0 || v > l) {
			return s, fmt.Errorf("field %q is %d, outside 0..%d", name, v, l)
		}
		switch name {
		case "s":
			s.Share = int(v)
		case "f":
			s.File = int(v)
		case "off":
			s.Off = v
		case "len":
			s.Len = v
		case "b":
			s.Byte = byte(v)
		case "pick":
			s.Pick = uint8(v)
		default:
			return s, fmt.Errorf("no field %q", name)
		}
	}
	if why := s.valid(); why != "" {
		return s, fmt.Errorf("%s", why)
	}
	return s, nil
}

// Sequence is a corpus file read back: its name and its steps.
type Sequence struct {
	Name  string
	Steps []Step
}

// ReadCorpus reads every corpus file of dir, those named *.txt, in name
// order. A missing directory holds no sequence.
func ReadCorpus(dir string) ([]Sequence, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	var out []Sequence
	for _, path := range names {
		text, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		steps, err := parseCorpus(text)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		out = append(out, Sequence{Name: filepath.Base(path), Steps: steps})
	}
	return out, nil
}
