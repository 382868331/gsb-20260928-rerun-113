# 设备自检运行平台

第一版提供与设备型号解耦的 Go 执行器：

- `selfcheck.Check[T]` 描述诊断项、前置条件、超时和依赖。
- `selfcheck.CheckSet[T]` 是型号提供的检查集，新型号只需提供自己的检查集。
- `selfcheck.Engine` 负责拓扑排序、依赖判定、取消、超时、重跑和历史报告。
- `virtual.Device` 是虚拟设备示例，区分通信失败、传感器缺失和读数越界。

## 状态语义

- `passed`：检查通过。
- `failed`：检查实际执行并失败，可通过 `Code` 区分具体故障。
- `blocked`：前置条件不满足或上游未通过；不会算作通过。
- `canceled`：外部上下文取消。
- `timed_out`：单项检查超过自身超时。
- `expired`：上游重跑后，受影响的旧下游结论过期。

重跑某个检查会保存新报告，并仅让传递依赖该检查的最新下游结论生成 `expired` 记录。所有旧报告仍保留在 `Store.History()` 中。

## 运行

```bash
go test ./...
go run ./cmd/selfcheck-demo
```
