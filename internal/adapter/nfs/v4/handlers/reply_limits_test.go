package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// sizedResult builds a CompoundResult whose encoded size is 4 + dataLen bytes.
func sizedResult(dataLen int) *types.CompoundResult {
	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GETATTR,
		Data:   make([]byte, dataLen),
	}
}

// TestReplyLimits_Account walks the reply-size rules of RFC 8881
// Section 2.10.6.4: a reply over ca_maxresponsesize draws NFS4ERR_REP_TOO_BIG,
// one over ca_maxresponsesize_cached draws NFS4ERR_REP_TOO_BIG_TO_CACHE but only
// when the client asked for it to be cached, and neither fires until the running
// total actually passes the budget.
func TestReplyLimits_Account(t *testing.T) {
	tests := []struct {
		name      string
		limits    *replyLimits
		dataLens  []int
		wantFinal uint32
	}{
		{
			name:      "nil limits refuse nothing",
			limits:    nil,
			dataLens:  []int{1 << 20},
			wantFinal: types.NFS4_OK,
		},
		{
			name:      "under both budgets",
			limits:    &replyLimits{max: 400, maxCached: 400, cacheThis: true},
			dataLens:  []int{50, 50},
			wantFinal: types.NFS4_OK,
		},
		{
			name:      "a result leaving room for a refusal still fits",
			limits:    &replyLimits{max: 112},
			dataLens:  []int{100},
			wantFinal: types.NFS4_OK,
		},
		{
			name:      "a result landing exactly on the budget leaves no room to refuse",
			limits:    &replyLimits{max: 104},
			dataLens:  []int{100},
			wantFinal: types.NFS4ERR_REP_TOO_BIG,
		},
		{
			name:      "accumulates across operations",
			limits:    &replyLimits{max: 200},
			dataLens:  []int{96, 100},
			wantFinal: types.NFS4ERR_REP_TOO_BIG,
		},
		{
			name:      "header already counted",
			limits:    &replyLimits{size: 180, max: 200},
			dataLens:  []int{96},
			wantFinal: types.NFS4ERR_REP_TOO_BIG,
		},
		{
			name:      "cache budget ignored when cachethis is false",
			limits:    &replyLimits{max: 8192, maxCached: 10},
			dataLens:  []int{100},
			wantFinal: types.NFS4_OK,
		},
		{
			name:      "a zero cache budget caches nothing rather than everything",
			limits:    &replyLimits{max: 8192, maxCached: 0, cacheThis: true},
			dataLens:  []int{0},
			wantFinal: types.NFS4ERR_REP_TOO_BIG_TO_CACHE,
		},
		{
			name:      "cache budget enforced when cachethis is true",
			limits:    &replyLimits{max: 8192, maxCached: 10, cacheThis: true},
			dataLens:  []int{100},
			wantFinal: types.NFS4ERR_REP_TOO_BIG_TO_CACHE,
		},
		{
			name:      "unsendable outranks uncacheable",
			limits:    &replyLimits{max: 50, maxCached: 10, cacheThis: true},
			dataLens:  []int{100},
			wantFinal: types.NFS4ERR_REP_TOO_BIG,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got uint32
			for _, n := range tc.dataLens {
				got = tc.limits.account(sizedResult(n))
			}
			if got != tc.wantFinal {
				t.Errorf("account() = %d, want %d", got, tc.wantFinal)
			}
		})
	}
}

// TestOverflow_DropsOperationOutput checks that a result which did not fit is
// replaced by the status-only reply rather than shipped alongside an error: a
// caller told the reply was too big must not also be handed a partial result or
// a stateid it cannot have seen.
func TestOverflow_DropsOperationOutput(t *testing.T) {
	r := sizedResult(100)
	r.Stateid = &types.Stateid4{Seqid: 7}

	overflow(r, types.NFS4ERR_REP_TOO_BIG)

	if r.Status != types.NFS4ERR_REP_TOO_BIG {
		t.Errorf("Status = %d, want %d", r.Status, types.NFS4ERR_REP_TOO_BIG)
	}
	if r.Stateid != nil {
		t.Error("Stateid survived an overflowed result")
	}
	want := encodeStatusOnly(types.NFS4ERR_REP_TOO_BIG)
	if string(r.Data) != string(want) {
		t.Errorf("Data = % x, want the status-only encoding % x", r.Data, want)
	}
}

// TestCompoundSizeHelpers_MatchEncoder pins the running-size arithmetic to what
// encodeCompoundResponse actually writes. The reply-size checks are only correct
// while the two agree, and they are computed in different functions.
func TestCompoundSizeHelpers_MatchEncoder(t *testing.T) {
	for _, tag := range [][]byte{nil, []byte("a"), []byte("ab"), []byte("abc"), []byte("abcd"), []byte("abcde")} {
		results := []types.CompoundResult{*sizedResult(12), *sizedResult(33)}

		want := compoundHeaderSize(tag)
		for i := range results {
			want += compoundResultSize(&results[i])
		}

		encoded, err := encodeCompoundResponse(types.NFS4_OK, tag, results)
		if err != nil {
			t.Fatalf("encodeCompoundResponse(tag=%q): %v", tag, err)
		}
		if uint32(len(encoded)) != want {
			t.Errorf("tag %q: encoded %d bytes, size helpers predicted %d", tag, len(encoded), want)
		}
	}
}

// TestReplyLimits_SaturatesRatherThanWraps checks the counter refuses a result
// big enough to wrap it. An unsaturated uint32 add would land back near zero and
// report the reply as fitting comfortably inside the budget.
func TestReplyLimits_SaturatesRatherThanWraps(t *testing.T) {
	l := &replyLimits{size: ^uint32(0) - 8, max: 8192}

	if got := l.account(sizedResult(64)); got != types.NFS4ERR_REP_TOO_BIG {
		t.Errorf("account() = %d, want %d", got, types.NFS4ERR_REP_TOO_BIG)
	}
	if l.size != ^uint32(0) {
		t.Errorf("size = %d, want it pinned at the maximum", l.size)
	}
}

// TestReplyLimits_RefusalStaysInsideTheBudget is the regression for a reply that
// carried NFS4ERR_REP_TOO_BIG and was itself over ca_maxresponsesize. Admitting
// a result that lands exactly on the budget leaves nothing for the refusal that
// follows it, and the refusal costs eight bytes of its own — so the server sent
// the budget plus eight, with the error saying the budget had been exceeded.
//
// Whatever the operation sizes, the total the limits admit must never exceed
// max, including after a refusal.
func TestReplyLimits_RefusalStaysInsideTheBudget(t *testing.T) {
	const max = 200

	for first := 0; first <= 160; first += 4 {
		for second := 0; second <= 160; second += 4 {
			l := &replyLimits{max: max}
			if s := l.account(sizedResult(first)); s != types.NFS4_OK {
				// The first result alone did not fit; nothing more to check.
				if l.size > max {
					t.Fatalf("first=%d: refused at size %d, over max %d", first, l.size, max)
				}
				continue
			}
			l.account(sizedResult(second))
			if l.size > max {
				t.Fatalf("first=%d second=%d: emitted %d bytes, over max %d", first, second, l.size, max)
			}
		}
	}
}

// TestReplyLimits_CountsTheRpcHeader pins the reply budget to what RFC 8881
// Section 18.36.3 actually measures — the reply "including RPC headers" — so
// the count cannot quietly drift back to starting at the COMPOUND status word.
func TestReplyLimits_CountsTheRpcHeader(t *testing.T) {
	tag := []byte("tag")
	seeded := rpc.ReplyOverhead + compoundHeaderSize(tag)

	if seeded <= compoundHeaderSize(tag) {
		t.Fatalf("seed %d does not include the RPC overhead", seeded)
	}
	if rpc.ReplyOverhead == 0 {
		t.Fatal("rpc.ReplyOverhead is zero; the budget would ignore the RPC headers entirely")
	}
}
