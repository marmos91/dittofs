package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// result builds a CompoundResult whose encoded size is 4 + dataLen bytes.
func result(dataLen int) *types.CompoundResult {
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
			name:      "exactly at the budget still fits",
			limits:    &replyLimits{max: 104},
			dataLens:  []int{100},
			wantFinal: types.NFS4_OK,
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
		{
			name:      "zero budget disables each check independently",
			limits:    &replyLimits{max: 0, maxCached: 0, cacheThis: true},
			dataLens:  []int{1 << 20},
			wantFinal: types.NFS4_OK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got uint32
			for _, n := range tc.dataLens {
				got = tc.limits.account(result(n))
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
	r := result(100)
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
		results := []types.CompoundResult{*result(12), *result(33)}

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
