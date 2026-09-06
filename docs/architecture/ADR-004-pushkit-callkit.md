# ADR-004：PushKit + CallKit 处理来电

状态：已接受

Gateway 是电话状态唯一真相源。来电 PushKit payload 只携带创建 CallKit UI 所需的最小元数据；iOS 收到 VoIP push 后必须立即 `reportNewIncomingCall`，不能等待完整 Gateway 连接后再报告。

