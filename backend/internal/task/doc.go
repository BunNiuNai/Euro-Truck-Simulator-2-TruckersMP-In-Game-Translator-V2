// Package task 后台任务队列：定时广告发送器、发送链路。
//
// V1 来源：main.py（_ad_*）+ compose_sender.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · AdSender 生命周期不得绑定任何 UI——V1 的缺陷是关掉设置窗口广告就停（D2）
//   · SendPipeline 互斥，并发时返回 BUSY（V1 SendResult 5 态语义保留）
//   · 发送确认通过回读聊天日志文件，不消费 engine 的消息队列
package task
