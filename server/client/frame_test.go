package client

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

// 心跳与版本协商解析的单测。这两条线出过错（心跳 ticker 从未创建、无心跳连接被读超时误杀），
// 边界值值得单独钉住。

// 心跳头的唯一解析入口：缺头按规范等同 "0,0"；格式非法或超上限都报错，
// 由调用方决定处置（handleConnect 据此回 ERROR，readLoop 按 0 处理）。
func TestParseHeartBeat(t *testing.T) {
	cases := []struct {
		name    string
		hb      string
		cx, cy  time.Duration
		wantErr error
	}{
		{"缺头按 0,0（规范）", "", 0, 0, nil},
		{"双零", "0,0", 0, 0, nil},
		{"只发不收", "10000,0", 10 * time.Second, 0, nil},
		{"只收不发", "0,10000", 0, 10 * time.Second, nil},
		{"正常", "5000,20000", 5 * time.Second, 20 * time.Second, nil},
		{"上限（maxHeartBeat）", "999999999,999999999", 999999999 * time.Millisecond, 999999999 * time.Millisecond, nil},
		{"前导零（仍是合法数字）", "0000000001,0", time.Millisecond, 0, nil},
		{"超上限", "9999999999,0", 0, 0, errHeartBeatTooLarge},
		{"超 64 位", strings.Repeat("9", 30) + ",0", 0, 0, errInvalidHeartBeat},
		{"畸形：非数字", "abc,10", 0, 0, errInvalidHeartBeat},
		{"畸形：缺一列", "10000", 0, 0, errInvalidHeartBeat},
		{"畸形：三列", "1,2,3", 0, 0, errInvalidHeartBeat},
		{"畸形：负数", "-1,10", 0, 0, errInvalidHeartBeat},
		{"畸形：空列", "10000,", 0, 0, errInvalidHeartBeat},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cx, cy, err := parseHeartBeat(tc.hb)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parseHeartBeat(%q) 错误 = %v, want %v", tc.hb, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if cx != tc.cx || cy != tc.cy {
				t.Errorf("parseHeartBeat(%q) = %v,%v, want %v,%v", tc.hb, cx, cy, tc.cx, tc.cy)
			}
		})
	}
}

// formatHeartBeat 与 parseHeartBeat 互逆（解析出来的值再格式化回同样的数字）。
func TestFormatHeartBeat(t *testing.T) {
	cases := []struct {
		cx, cy time.Duration
		want   string
	}{
		{0, 0, "0,0"},
		{30 * time.Second, 0, "30000,0"},
		{0, 30 * time.Second, "0,30000"},
		{5 * time.Second, 20 * time.Second, "5000,20000"},
	}
	for _, tc := range cases {
		if got := formatHeartBeat(tc.cx, tc.cy); got != tc.want {
			t.Errorf("formatHeartBeat(%v, %v) = %q, want %q", tc.cx, tc.cy, got, tc.want)
		}
	}
}

// 版本协商：取双方共有里的最高版本；缺头按 1.0（规范原文），完全无交集才报错。
func TestNegotiateVersion(t *testing.T) {
	cases := []struct {
		name          string
		acceptVersion string
		want          stomp.Version
		wantErr       bool
	}{
		{"缺头按 1.0", "", stomp.V10, false},
		{"单版本 1.2", "1.2", stomp.V12, false},
		{"取最高的共有版本", "1.0,1.1,1.2", stomp.V12, false},
		{"乱序也取最高", "1.1,1.2,1.0", stomp.V12, false},
		{"容忍空白", "1.0, 1.1, 1.2", stomp.V12, false},
		{"只要 1.0", "1.0", stomp.V10, false},
		{"只要 1.1", "1.1", stomp.V11, false},
		{"未知版本不算共有", "2.0", "", true},
		{"有交集就不报错", "2.0,1.1", stomp.V11, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := negotiateVersion(tc.acceptVersion)
			if (err != nil) != tc.wantErr {
				t.Fatalf("negotiateVersion(%q) 错误 = %v, wantErr %v", tc.acceptVersion, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("negotiateVersion(%q) = %q, want %q", tc.acceptVersion, got, tc.want)
			}
		})
	}
}

// CONNECTED 的头按 STOMP 1.2 不做转义，frame 包因此会先校验头值。
// 非法头值必须作为错误交出来 —— 否则 buffer 是空的，writeLoop 会静默发一个空帧。
func TestFrameToBytesReportsUnescapedHeaderError(t *testing.T) {
	f := frame.New(frame.CONNECTED, frame.Session, "bad\nsession")

	_, err := frameToBytes(f)

	if !errors.Is(err, frame.ErrInvalidFrameFormat) {
		t.Fatalf("frameToBytes() error = %v, want %v", err, frame.ErrInvalidFrameFormat)
	}
}

// 投递前分配 message-id 时，ack 头要按订阅的 ack 模式定：非 auto 才给（值即 message-id），
// auto 必须摘掉 —— 留着它等于告诉一个 auto 订阅「要确认」。帧可能是应用自建的，
// 不能假定它没带 ack 头（上游两条分支都摘）。
