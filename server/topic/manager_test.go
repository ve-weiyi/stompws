package topic

import "testing"

// Find 按目的地创建 topic，并对同一目的地返回同一个实例。
func TestManagerFind(t *testing.T) {
	mgr := NewManager()

	t1 := mgr.Find("topic1")
	if t1 == nil {
		t.Fatal("Find(topic1) = nil, want a topic")
	}

	t2 := mgr.Find("topic2")
	if t2 == nil {
		t.Fatal("Find(topic2) = nil, want a topic")
	}

	if again := mgr.Find("topic1"); again != t1 {
		t.Errorf("Find(topic1) 第二次 = %p, want 同一个实例 %p", again, t1)
	}
}
