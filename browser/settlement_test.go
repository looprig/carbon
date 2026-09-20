package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/sessionstore"
)

type pendingPages struct {
	shards int
	pages  map[int][]sessionstore.DispositionDueCommandPage
	reads  map[int]int
}

func (p *pendingPages) ControlShards() int { return p.shards }
func (p *pendingPages) ListDueDispositionCommands(_ context.Context, req sessionstore.ListDueDispositionCommandsRequest) (sessionstore.DispositionDueCommandPage, error) {
	if p.reads == nil {
		p.reads = make(map[int]int)
	}
	index := p.reads[req.Shard]
	p.reads[req.Shard]++
	if index >= len(p.pages[req.Shard]) {
		return sessionstore.DispositionDueCommandPage{}, nil
	}
	if index == 0 && req.DueAtOrBefore != time.Unix(0, 1<<63-1).UTC() {
		return sessionstore.DispositionDueCommandPage{}, errors.New("scan did not include all deadlines")
	}
	if index > 0 && (req.Cursor == "" || !req.DueAtOrBefore.IsZero()) {
		return sessionstore.DispositionDueCommandPage{}, errors.New("bad continuation")
	}
	return p.pages[req.Shard][index], nil
}

func pendingFor(tenant sessionwire.TenantID) sessionstore.DispositionInboxEntry {
	return sessionstore.DispositionInboxEntry{Record: sessionstore.DispositionInboxRecord{Descriptor: sessionstore.DispositionCommandDescriptor{TenantID: tenant}}}
}

func TestScanOutstandingPagesEveryShardAndScopesTenant(t *testing.T) {
	p := &pendingPages{shards: 2, pages: map[int][]sessionstore.DispositionDueCommandPage{
		0: {{Commands: []sessionstore.DispositionInboxEntry{pendingFor("other")}, NextCursor: "next"}, {Commands: []sessionstore.DispositionInboxEntry{pendingFor("local")}}},
		1: {{Commands: []sessionstore.DispositionInboxEntry{pendingFor("local")}}},
	}}
	count, err := scanOutstanding(context.Background(), p, "local", 8)
	if err != nil || count != 2 || p.reads[0] != 2 || p.reads[1] != 1 {
		t.Fatalf("scan = (%d, %v), reads = %v", count, err, p.reads)
	}
}

func TestScanOutstandingRefusesUnreadableAndBudgetExhaustion(t *testing.T) {
	for _, page := range []sessionstore.DispositionDueCommandPage{{Unreadable: 1}, {NextCursor: "again"}} {
		p := &pendingPages{shards: 1, pages: map[int][]sessionstore.DispositionDueCommandPage{0: {page}}}
		_, err := scanOutstanding(context.Background(), p, "local", 1)
		var incomplete *PendingObservationError
		if !errors.As(err, &incomplete) {
			t.Fatalf("page %+v scan = %v, want typed incomplete", page, err)
		}
	}
}

func TestWaitOutstandingKeepsNonterminalCommandsUntilCleanPass(t *testing.T) {
	p := &pendingPages{shards: 1, pages: map[int][]sessionstore.DispositionDueCommandPage{0: {{Commands: []sessionstore.DispositionInboxEntry{pendingFor("local")}}}}}
	waits := 0
	err := waitOutstanding(context.Background(), p, "local", 8, func(context.Context) error {
		waits++
		p.pages[0] = nil
		p.reads = nil
		return nil
	})
	if err != nil || waits != 1 {
		t.Fatalf("wait = %v, polls = %d", err, waits)
	}
}

func TestWaitOutstandingDeadlineKeepsPendingTyped(t *testing.T) {
	p := &pendingPages{shards: 1, pages: map[int][]sessionstore.DispositionDueCommandPage{0: {{Commands: []sessionstore.DispositionInboxEntry{pendingFor("local")}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	err := waitOutstanding(ctx, p, "local", 8, func(context.Context) error { cancel(); return ctx.Err() })
	var pending *PendingCommandsError
	if !errors.As(err, &pending) || pending.Count != 1 {
		t.Fatalf("wait = %v, want one pending command", err)
	}
}
