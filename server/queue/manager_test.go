package queue

import "testing"

// Find 按目的地创建队列，并对同一目的地返回同一个实例。
func TestManagerFind(t *testing.T) {
	mgr := NewManager(NewMemoryQueueStorage())

	q1 := mgr.Find("/queue/1")
	if q1 == nil {
		t.Fatal("Find(/queue/1) = nil, want a queue")
	}

	q2 := mgr.Find("/queue/2")
	if q2 == nil {
		t.Fatal("Find(/queue/2) = nil, want a queue")
	}

	if again := mgr.Find("/queue/1"); again != q1 {
		t.Errorf("Find(/queue/1) 第二次 = %p, want 同一个实例 %p", again, q1)
	}
}
