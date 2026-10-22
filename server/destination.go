package server

import "strings"

// 目的地前缀。STOMP 规范把 destination 当作**不透明字符串**（"opaque string"），
// 投递语义由服务端自定；/topic/（一对多广播）与 /queue/（一条只给一个消费者）
// 是本服务选定的约定，与 ActiveMQ/RabbitMQ 的事实约定一致。
//
// 判定一律走下面两个函数：散落的 HasPrefix 一旦写法不一致，就会出现
// 「校验放过去、中继却不认」的目的地（例如把 "/queue" 当成前缀会误吃 "/queuefoo"）。
const (
	topicPrefix = "/topic/"
	queuePrefix = "/queue/"
)

// isTopicDestination 判断是否广播目的地。
func isTopicDestination(dest string) bool {
	return strings.HasPrefix(dest, topicPrefix)
}

// isQueueDestination 判断是否点对点目的地。
func isQueueDestination(dest string) bool {
	return strings.HasPrefix(dest, queuePrefix)
}
