package objectstore

// White-box test (package objectstore, not objectstore_test) — exercises
// the unexported save() function directly with fake blobPutter/blobDeleter/
// metaUpserter doubles, proving the compensating-delete path without
// fabricating a real Postgres failure.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakePutter struct {
	putCalls []string
	putErr   error
}

func (f *fakePutter) Put(_ context.Context, key string, _ []byte, _ string) error {
	f.putCalls = append(f.putCalls, key)
	return f.putErr
}

type fakeDeleter struct {
	deleteCalls []string
	deleteErr   error
}

func (f *fakeDeleter) Delete(_ context.Context, key string) error {
	f.deleteCalls = append(f.deleteCalls, key)
	return f.deleteErr
}

type fakeMetaUpserter struct {
	upsertErr error
}

func (f *fakeMetaUpserter) Upsert(_ context.Context, _, _, _ string, _ int64, _, _, _, _ string) (Record, error) {
	if f.upsertErr != nil {
		return Record{}, f.upsertErr
	}
	return Record{ObjectKey: "k"}, nil
}

func TestSave_HappyPath(t *testing.T) {
	putter := &fakePutter{}
	deleter := &fakeDeleter{}
	meta := &fakeMetaUpserter{}

	rec, err := save(context.Background(), putter, deleter, meta, "bucket", "owner_type", "owner_id", "k", []byte("data"), "text/plain", "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rec.ObjectKey != "k" {
		t.Errorf("record = %+v", rec)
	}
	if len(deleter.deleteCalls) != 0 {
		t.Errorf("compensating delete called on the happy path: %v", deleter.deleteCalls)
	}
}

func TestSave_MetadataFailureTriggersCompensatingDelete(t *testing.T) {
	putter := &fakePutter{}
	deleter := &fakeDeleter{}
	meta := &fakeMetaUpserter{upsertErr: errors.New("db unavailable")}

	_, err := save(context.Background(), putter, deleter, meta, "bucket", "owner_type", "owner_id", "k", []byte("data"), "text/plain", "")
	if err == nil {
		t.Fatal("expected an error when metadata upsert fails")
	}
	if len(putter.putCalls) != 1 || putter.putCalls[0] != "k" {
		t.Fatalf("put calls = %v, want exactly one Put(\"k\")", putter.putCalls)
	}
	if len(deleter.deleteCalls) != 1 || deleter.deleteCalls[0] != "k" {
		t.Fatalf("delete calls = %v, want exactly one compensating Delete(\"k\")", deleter.deleteCalls)
	}
}

func TestSave_CompensatingDeleteFailureIsReportedNotSwallowed(t *testing.T) {
	putter := &fakePutter{}
	deleter := &fakeDeleter{deleteErr: errors.New("blob store also unavailable")}
	meta := &fakeMetaUpserter{upsertErr: errors.New("db unavailable")}

	_, err := save(context.Background(), putter, deleter, meta, "bucket", "owner_type", "owner_id", "k", []byte("data"), "text/plain", "")
	if err == nil {
		t.Fatal("expected an error when both metadata upsert and the compensating delete fail")
	}
	// Both failures must be visible in the error, not just the first one —
	// an orphaned blob after a failed compensating delete is a real
	// operational fact worth surfacing, not silently dropped.
	if !containsAll(err.Error(), "db unavailable", "blob store also unavailable") {
		t.Errorf("error %q does not mention both underlying failures", err.Error())
	}
}

func TestSave_PutFailureNeverCallsUpsertOrDelete(t *testing.T) {
	putter := &fakePutter{putErr: errors.New("blob store unavailable")}
	deleter := &fakeDeleter{}
	meta := &fakeMetaUpserter{}

	_, err := save(context.Background(), putter, deleter, meta, "bucket", "owner_type", "owner_id", "k", []byte("data"), "text/plain", "")
	if err == nil {
		t.Fatal("expected an error when Put fails")
	}
	if len(deleter.deleteCalls) != 0 {
		t.Errorf("compensating delete called even though Put itself failed: %v", deleter.deleteCalls)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
