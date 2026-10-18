package logctx

import (
	"context"
	"sync"
	"testing"
)

func TestWithAndIDs(t *testing.T) {
	ctx := Begin(context.Background())
	ctx = With(ctx, "request_id", "req_1", "payment_id", "pay_1", "empty", "", "odd")
	got := IDs(ctx)
	if got["request_id"] != "req_1" || got["payment_id"] != "pay_1" {
		t.Errorf("%v", got)
	}
	if _, ok := got["empty"]; ok {
		t.Error("empty values are not kept")
	}
	got["request_id"] = "changed"
	if IDs(ctx)["request_id"] != "req_1" {
		t.Error("IDs returns a copy")
	}
}

func TestNoteReachesTheRequestScopeFromDeeperCode(t *testing.T) {
	root := Begin(context.Background())
	child := context.WithValue(root, struct{}{}, 1) // a handler's derived context
	Note(child, "org_id", "org_1")
	if IDs(root)["org_id"] != "org_1" {
		t.Errorf("%v", IDs(root))
	}
}

func TestNoteWithoutAScopeIsANoOp(t *testing.T) {
	ctx := context.Background()
	Note(ctx, "a", "b")
	if len(IDs(ctx)) != 0 {
		t.Error("no scope, no ids")
	}
	if got := IDs(With(ctx, "a", "b")); got["a"] != "b" {
		t.Errorf("With creates a scope: %v", got)
	}
}

func TestBeginStartsAFreshScope(t *testing.T) {
	parent := With(context.Background(), "request_id", "req_1")
	job := Begin(parent)
	With(job, "job_id", "job_1")
	if _, ok := IDs(parent)["job_id"]; ok {
		t.Error("a job's ids leak into the request that enqueued it")
	}
	if IDs(job)["job_id"] != "job_1" {
		t.Error("job scope")
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx := Begin(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); Note(ctx, "k", "v"); _ = IDs(ctx) }()
	}
	wg.Wait()
}
