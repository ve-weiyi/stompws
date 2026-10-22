package client

import (
	"errors"
	"testing"

	"github.com/go-stomp/stomp/v3/frame"
)

// txStore 的单测：Begin 的幂等约束、Add 摘掉 transaction 头、Commit 按序重放并清空、
// Abort 丢弃、未知事务报错、断开时 Clear。
//

func txFrame(dest string) *frame.Frame {
	return frame.New(frame.MESSAGE, frame.Destination, dest)
}

// 同一个事务名不能重复 BEGIN。
func TestTxStoreBeginTwiceFails(t *testing.T) {
	txs := newTxStore()

	if err := txs.Begin("tx1"); err != nil {
		t.Fatalf("Begin(tx1) = %v, want nil", err)
	}
	if err := txs.Begin("tx1"); !errors.Is(err, errTransactionExists) {
		t.Errorf("重复 Begin(tx1) = %v, want %v", err, errTransactionExists)
	}
	// 另一个事务名互不影响。
	if err := txs.Begin("tx2"); err != nil {
		t.Errorf("Begin(tx2) = %v, want nil", err)
	}
}

// Add 必须摘掉 transaction 头：提交时这些帧会被**重新走一遍 handleFrame**，
// 残留的头会让它们又进一次事务（导致提交后不生效）。
func TestTxStoreAddStripsTransactionHeader(t *testing.T) {
	txs := newTxStore()
	if err := txs.Begin("tx1"); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	f := frame.New(frame.MESSAGE, frame.Destination, "/queue/a", frame.Transaction, "tx1")
	if err := txs.Add("tx1", f); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, ok := f.Header.Contains(frame.Transaction); ok {
		t.Errorf("Add 之后帧上仍有 transaction 头 = %q, want 已摘掉", f.Header.Get(frame.Transaction))
	}
}

// Commit 按加入顺序把帧交给回调，之后事务消失（同一个事务不能再提交）。
func TestTxStoreCommitReplaysInOrder(t *testing.T) {
	txs := newTxStore()
	for _, tx := range []string{"tx1", "tx2"} {
		if err := txs.Begin(tx); err != nil {
			t.Fatalf("Begin(%s): %v", tx, err)
		}
	}

	f1, f2, f3, f4 := txFrame("/queue/1"), txFrame("/queue/2"), txFrame("/queue/3"), txFrame("/queue/4")
	for _, f := range []*frame.Frame{f1, f2, f3} {
		if err := txs.Add("tx1", f); err != nil {
			t.Fatalf("Add(tx1): %v", err)
		}
	}
	if err := txs.Add("tx2", f4); err != nil {
		t.Fatalf("Add(tx2): %v", err)
	}

	var tx1, tx2 []*frame.Frame
	if err := txs.Commit("tx1", func(f *frame.Frame) error { tx1 = append(tx1, f); return nil }); err != nil {
		t.Fatalf("Commit(tx1): %v", err)
	}
	if err := txs.Commit("tx2", func(f *frame.Frame) error { tx2 = append(tx2, f); return nil }); err != nil {
		t.Fatalf("Commit(tx2): %v", err)
	}

	if len(tx1) != 3 || tx1[0] != f1 || tx1[1] != f2 || tx1[2] != f3 {
		t.Errorf("tx1 重放 %v, want [f1 f2 f3]（按加入顺序）", tx1)
	}
	if len(tx2) != 1 || tx2[0] != f4 {
		t.Errorf("tx2 重放 %v, want [f4]", tx2)
	}

	// 已提交的事务不再存在：回调不该被调用。
	if err := txs.Commit("tx1", func(*frame.Frame) error {
		t.Error("已提交的事务又调用了回调")
		return nil
	}); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("再次 Commit(tx1) = %v, want %v", err, errTransactionNotFound)
	}
}

// 回调返回错误时把错误交出来（调用方据此回 ERROR）。
func TestTxStoreCommitPropagatesCallbackError(t *testing.T) {
	txs := newTxStore()
	if err := txs.Begin("tx1"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := txs.Add("tx1", txFrame("/queue/a")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	wantErr := errors.New("replay failed")
	if err := txs.Commit("tx1", func(*frame.Frame) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("Commit 回调报错时 = %v, want %v", err, wantErr)
	}
}

// Abort 丢掉事务里的全部帧，且事务不再存在。
func TestTxStoreAbortDropsFrames(t *testing.T) {
	txs := newTxStore()
	if err := txs.Begin("tx1"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := txs.Add("tx1", txFrame("/queue/a")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := txs.Abort("tx1"); err != nil {
		t.Fatalf("Abort(tx1) = %v, want nil", err)
	}
	if err := txs.Commit("tx1", func(*frame.Frame) error {
		t.Error("已 Abort 的事务又调用了回调")
		return nil
	}); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("Abort 之后 Commit = %v, want %v", err, errTransactionNotFound)
	}
	if err := txs.Abort("tx1"); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("重复 Abort = %v, want %v", err, errTransactionNotFound)
	}
}

// 未 BEGIN 的事务：Add / Commit / Abort 都要报错（而不是静默建一个事务）。
func TestTxStoreUnknownTransaction(t *testing.T) {
	txs := newTxStore()

	if err := txs.Add("nope", txFrame("/queue/a")); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("Add(未知事务) = %v, want %v", err, errTransactionNotFound)
	}
	if err := txs.Commit("nope", func(*frame.Frame) error {
		t.Error("未知事务调用了回调")
		return nil
	}); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("Commit(未知事务) = %v, want %v", err, errTransactionNotFound)
	}
	if err := txs.Abort("nope"); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("Abort(未知事务) = %v, want %v", err, errTransactionNotFound)
	}
}

// Clear 丢掉全部在途事务（断开连接时调用）。
func TestTxStoreClear(t *testing.T) {
	txs := newTxStore()
	if err := txs.Begin("tx1"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := txs.Add("tx1", txFrame("/queue/a")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	txs.Clear()

	if err := txs.Commit("tx1", func(*frame.Frame) error {
		t.Error("Clear 之后回调仍被调用")
		return nil
	}); !errors.Is(err, errTransactionNotFound) {
		t.Errorf("Clear 之后 Commit = %v, want %v", err, errTransactionNotFound)
	}
}
