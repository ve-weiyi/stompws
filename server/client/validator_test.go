package client

// validator.go 的规则表：每个用例钉一条「必填头 / 不许有的头 / 合法性」，
// 并带上协商出的版本 —— 规则随版本变化（1.1+ 要 host、1.2 的 ACK 用 id）。
//
// 心跳格式不在这里判：它由 frame.go 的解析入口判定（见该文件的用例）。

import (
	"strings"
	"testing"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

func TestValidator(t *testing.T) {
	cases := []struct {
		name    string
		version stomp.Version
		command string
		headers []string // frame.New 的 key,value 对
		wantErr string   // 空 = 应通过；否则是错误文案里应含的片段
	}{
		// ── CONNECT / STOMP ──
		{"CONNECT 齐备", stomp.V12, frame.CONNECT, []string{frame.AcceptVersion, "1.2", frame.Host, "/"}, ""},
		{"CONNECT 缺 accept-version", stomp.V12, frame.CONNECT, []string{frame.Host, "/"}, frame.AcceptVersion},
		{"CONNECT 缺 host", stomp.V12, frame.CONNECT, []string{frame.AcceptVersion, "1.2"}, frame.Host},
		{"CONNECT 不许带 receipt", stomp.V12, frame.CONNECT,
			[]string{frame.AcceptVersion, "1.2", frame.Host, "/", frame.Receipt, "r"}, "receipt"},
		{"STOMP 帧与 CONNECT 同规则", stomp.V12, frame.STOMP, []string{frame.AcceptVersion, "1.2", frame.Host, "/"}, ""},
		{"1.0 的 CONNECT 不要求 accept-version/host", stomp.V10, frame.CONNECT, nil, ""},
		{"CONNECT 的心跳格式不在这里判", stomp.V12, frame.CONNECT,
			[]string{frame.AcceptVersion, "1.2", frame.Host, "/", frame.HeartBeat, "abc"}, ""},

		// ── SEND ──
		{"SEND 齐备", stomp.V12, frame.SEND, []string{frame.Destination, "/queue/a"}, ""},
		{"SEND 缺 destination", stomp.V12, frame.SEND, nil, frame.Destination},
		{"SEND 目的地含非法字符", stomp.V12, frame.SEND, []string{frame.Destination, "/queue/a b"}, "destination"},
		{"SEND 目的地缺前导斜杠", stomp.V12, frame.SEND, []string{frame.Destination, "queue/a"}, "destination"},

		// ── SUBSCRIBE / UNSUBSCRIBE ──
		{"SUBSCRIBE 齐备", stomp.V12, frame.SUBSCRIBE, []string{frame.Destination, "/topic/a", frame.Id, "s1"}, ""},
		{"SUBSCRIBE 缺 id", stomp.V12, frame.SUBSCRIBE, []string{frame.Destination, "/topic/a"}, "header: id"},
		{"SUBSCRIBE 缺 destination", stomp.V12, frame.SUBSCRIBE, []string{frame.Id, "s1"}, frame.Destination},
		// ack 取值必须在订阅时就拦住：非法值会让 IsAckedBy 永不匹配，
		// 帧卡在 subList 里、队列订阅再不挂回 —— 客户端却一路收到 RECEIPT。
		{"SUBSCRIBE 的 ack 取值合法（client-individual）", stomp.V12, frame.SUBSCRIBE,
			[]string{frame.Destination, "/queue/a", frame.Id, "s1", frame.Ack, frame.AckClientIndividual}, ""},
		{"SUBSCRIBE 的 ack 取值合法（client）", stomp.V11, frame.SUBSCRIBE,
			[]string{frame.Destination, "/queue/a", frame.Id, "s1", frame.Ack, frame.AckClient}, ""},
		{"SUBSCRIBE 的 ack 取值非法", stomp.V12, frame.SUBSCRIBE,
			[]string{frame.Destination, "/queue/a", frame.Id, "s1", frame.Ack, "bogus"}, "ack"},
		{"SUBSCRIBE 的 ack 为空等同 auto", stomp.V12, frame.SUBSCRIBE,
			[]string{frame.Destination, "/queue/a", frame.Id, "s1", frame.Ack, ""}, ""},
		{"UNSUBSCRIBE 齐备", stomp.V12, frame.UNSUBSCRIBE, []string{frame.Id, "s1"}, ""},
		{"UNSUBSCRIBE 缺 id", stomp.V12, frame.UNSUBSCRIBE, nil, "header: id"},

		// ── ACK / NACK ──
		{"1.2 的 ACK 用 id", stomp.V12, frame.ACK, []string{frame.Id, "7"}, ""},
		{"1.2 的 ACK 缺 id", stomp.V12, frame.ACK, nil, "header: id"},
		{"1.1 的 ACK 用 message-id", stomp.V11, frame.ACK, []string{frame.MessageId, "7"}, ""},
		{"1.1 的 ACK 也可以用 ack", stomp.V11, frame.ACK, []string{frame.Ack, "7"}, ""},
		{"1.1 的 ACK 两者都缺", stomp.V11, frame.ACK, nil, "message-id"},
		{"1.2 的 NACK 用 id", stomp.V12, frame.NACK, []string{frame.Id, "7"}, ""},
		{"1.0 不支持 NACK", stomp.V10, frame.NACK, []string{frame.MessageId, "7"}, "NACK"},

		// ── 事务 ──
		{"BEGIN 齐备", stomp.V12, frame.BEGIN, []string{frame.Transaction, "tx"}, ""},
		{"COMMIT 缺 transaction", stomp.V12, frame.COMMIT, nil, frame.Transaction},
		{"ABORT 缺 transaction", stomp.V12, frame.ABORT, nil, frame.Transaction},

		// ── 没有额外规则的帧 ──
		{"DISCONNECT 无需头部", stomp.V12, frame.DISCONNECT, nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := newValidator(tc.version).Validate(frame.New(tc.command, tc.headers...))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr == "":
			case err == nil:
				t.Fatalf("Validate() = nil, want 含 %q 的错误", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Validate() = %v, want 含 %q", err, tc.wantErr)
			}
		})
	}
}
