package client

// frame.go 装帧编解码（frameToBytes）与协商字段的解析（版本、心跳）：解析读的是 header 值，
// 命令合法性由分发保证。

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

// frameToBytes 编码一个帧。CONNECT/CONNECTED 的头不转义，其合法性由 frame 包在写出前校验。
func frameToBytes(f *frame.Frame) ([]byte, error) {
	var buf bytes.Buffer
	if err := frame.NewWriter(&buf).Write(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const (
	// maxHeartBeat 是心跳值的上限（毫秒，约 11.5 天）。规范不规定上限，这里设一个：
	// 更大的值只可能来自坏客户端或误配置，直接拒掉比静默截断清楚。
	maxHeartBeat = 999999999
)

var (
	// heartBeatRegexp 只做语法检查（两段十进制数字），范围检查走数值比较 ——
	// 上限不该藏在正则的 {1,9} 里，否则改一下正则就悄悄改了策略。
	heartBeatRegexp = regexp.MustCompile(`^[0-9]+,[0-9]+$`)
)

// negotiateVersion 协商 STOMP 协议版本：取双方共有里的最高版本。
// 缺 accept-version 按规范视为 1.0（只适用于 CONNECT，判定在 handleConnect）。
func negotiateVersion(acceptVersion string) (stomp.Version, error) {
	if acceptVersion == "" {
		return stomp.V10, nil
	}

	clientVersions := strings.Split(acceptVersion, ",")
	for _, supported := range []stomp.Version{stomp.V12, stomp.V11, stomp.V10} {
		for _, client := range clientVersions {
			if strings.TrimSpace(client) == supported.String() {
				return supported, nil
			}
		}
	}

	return "", errUnsupportedVersion
}

// parseHeartBeat 解析 heart-beat 头 "cx,cy"。缺头按规范等同 "0,0"；
// 格式非法或超出 maxHeartBeat 时报错，由调用方决定处置。
func parseHeartBeat(hb string) (cx, cy time.Duration, err error) {
	if hb == "" {
		return 0, 0, nil
	}

	// 先做语法检查，范围另判（见下）
	if !heartBeatRegexp.MatchString(hb) {
		return 0, 0, errInvalidHeartBeat
	}

	parts := strings.Split(hb, ",")
	cxMs, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, errInvalidHeartBeat
	}
	cyMs, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, 0, errInvalidHeartBeat
	}
	if cxMs > maxHeartBeat || cyMs > maxHeartBeat {
		return 0, 0, errHeartBeatTooLarge
	}

	return time.Duration(cxMs) * time.Millisecond, time.Duration(cyMs) * time.Millisecond, nil
}

// formatHeartBeat 格式化 heart-beat 头部
func formatHeartBeat(cx, cy time.Duration) string {
	return fmt.Sprintf("%d,%d", cx/time.Millisecond, cy/time.Millisecond)
}
