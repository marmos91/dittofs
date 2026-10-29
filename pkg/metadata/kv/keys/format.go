package keys

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Describe renders a key for logs, debuggers and a scan tool: its kind and
// sub-kind by name and its components by type, joined by ‖, as in
// F‖share=3fa2c1d0‖file=9b1e44a7‖e‖2882400001‖"README.md". An ID shows its
// first four bytes, enough to tell keys apart by eye. A component Describe has
// no layout for shows as hex, so every key renders, if not every one by name.
func Describe(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	name, ok := kindNames[key[0]]
	if !ok {
		return "?" + hex.EncodeToString(key)
	}
	parts := []string{name}
	rest := key[1:]
	take := func(spec string) bool {
		var ok bool
		parts, rest, ok = render(parts, rest, spec)
		return ok
	}
	switch key[0] {
	case KindStore:
		parts, rest = append(parts, strconv.Quote(string(rest))), nil
	case KindF:
		if take("share file") && len(rest) > 0 {
			sub := rest[0]
			parts, rest = append(parts, subName(fSubNames, sub)), rest[1:]
			take(fSubLayouts[sub])
			if sub == FOp && len(rest) > 0 && rest[0] == OpL {
				parts, rest = append(parts, "l"), rest[1:]
				take("u u")
			}
		}
	case KindS:
		if take("share") && len(rest) > 0 {
			sub := rest[0]
			parts, rest = append(parts, subName(sSubNames, sub)), rest[1:]
			take(sSubLayouts[sub])
		}
	case KindC, KindBD, KindI:
		take("ns h")
	case KindCR:
		take("ns h id id u u")
	case KindB:
		take("ns h")
	case KindBR:
		take("ns u h")
	case KindCL:
		if take("u") && len(rest) > 0 && rest[0] == CLSh {
			parts, rest = append(parts, "sh"), rest[1:]
			take("u")
		}
	case KindCLO:
		take("b s")
	case KindSH:
		take("u")
	case KindIDS:
		take("b")
	}
	if len(rest) > 0 {
		parts = append(parts, hex.EncodeToString(rest))
	}
	return strings.Join(parts, "‖")
}

// render appends to parts each component spec names, read from b: share,
// file, id and ns a 16-byte ID, v a 16-byte version, h 32 bytes, u a uint64,
// b one byte, s an escaped string. It stops at the first component b cannot
// hold, leaving the rest to show as hex.
func render(parts []string, b []byte, spec string) ([]string, []byte, bool) {
	for _, c := range strings.Fields(spec) {
		switch c {
		case "share", "file", "id", "ns":
			if len(b) < IDLen {
				return parts, b, false
			}
			parts, b = append(parts, c+"="+hex.EncodeToString(b[:4])), b[IDLen:]
		case "v":
			if len(b) < 16 {
				return parts, b, false
			}
			v := binary.BigEndian.Uint64(b[8:16])
			if epoch := binary.BigEndian.Uint64(b[:8]); epoch != 0 {
				parts = append(parts, fmt.Sprintf("v%d.%d", epoch, v))
			} else {
				parts = append(parts, fmt.Sprintf("v%d", v))
			}
			b = b[16:]
		case "h":
			if len(b) < 32 {
				return parts, b, false
			}
			parts, b = append(parts, hex.EncodeToString(b[:4])+"…"), b[32:]
		case "u":
			if len(b) < 8 {
				return parts, b, false
			}
			parts, b = append(parts, strconv.FormatUint(binary.BigEndian.Uint64(b[:8]), 10)), b[8:]
		case "b":
			if len(b) < 1 {
				return parts, b, false
			}
			parts, b = append(parts, strconv.Itoa(int(b[0]))), b[1:]
		case "s":
			s, rest, err := ReadString(b)
			if err != nil {
				return parts, b, false
			}
			parts, b = append(parts, strconv.Quote(string(s))), rest
		}
	}
	return parts, b, true
}

func subName(names map[byte]string, sub byte) string {
	if n, ok := names[sub]; ok {
		return n
	}
	return fmt.Sprintf("0x%02x", sub)
}

var kindNames = map[byte]string{
	KindStore: "", KindF: "F", KindS: "S",
	KindC: "C", KindCR: "CR", KindB: "B", KindBR: "BR", KindBD: "BD", KindBC: "BC", KindI: "I", KindNS: "NS",
	KindU: "U", KindG: "G", KindM: "M", KindMR: "MR", KindGR: "GR", KindNX: "NX", KindNP: "NP", KindNG: "NG",
	KindSL: "SL", KindIN: "IN", KindNSM: "NSM", KindPX: "PX", KindN: "N", KindSH: "SH", KindJ: "J", KindRS: "RS",
	KindMV: "MV", KindSLOT: "SLOT", KindCFG: "CFG", KindCFGGEN: "CFGGEN", KindSEC: "SEC", KindCL: "CL",
	KindCLO: "CLO", KindTG: "TG", KindMKP: "MKP", KindIDS: "IDS",
}

var fSubNames = map[byte]string{
	FAcl: "acl", FX: "x", FS: "s", FE: "e", FT: "t", FH: "h", FRm: "rm", FOv: "ov", FRef: "ref",
	FHis: "H", FFx: "fx", FFo: "fo", FRel: "rel", FOp: "op", FDp: "dp",
}

var fSubLayouts = map[byte]string{
	FX: "s", FS: "id", FE: "u s", FT: "h", FH: "u", FRm: "v", FOv: "u", FRef: "u", FOp: "u",
}

var sSubNames = map[byte]string{
	SInfo: "info", SSt: "st", SNsg: "nsg", SG: "g", SXp: "xp", SSnap: "snap", SCut: "cut", SLive: "live",
	SSc: "sc", SHd: "hd", SHr: "hr", SUse: "use", SRh: "rh", SOr: "or", SUt: "ut", SPu: "pu", SPj: "pj",
	SUd: "ud", SQt: "qt", SQx: "qx", SVf: "vf", SFnc: "fnc", SVl: "vl", SPr: "pr", SRr: "rr", SDo: "do",
}

var sSubLayouts = map[byte]string{
	SUd: "u h", SPr: "u file", SRr: "u file v", SDo: "u file u",
}
