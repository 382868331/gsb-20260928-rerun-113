# 设备自检执行器（devicecheck）

按依赖拓扑执行设备自检项，区分三类失败、处理取消/超时、支持“失败项 + 受影响下游”重跑，并保留完整历史批次。

## 快速开始

```bash
go build ./...
go test -race ./...

# 可运行演示（虚拟设备），默认 all 依次演示四个场景
go run ./cmd/devicecheck [missing|slow|stuck|range|all]
```

- `missing`：电源传感器不存在 → 安装恢复 → 只重跑 `power` 及其下游。
- `slow`：温度慢响应超时、100ms 后外部取消、恢复后只重跑 `temperature` 及下游，并校验资源收尾。
- `stuck`：设备对取消无响应——检查按 `timeout` 落结论，但在途读取尚未收回（`cleanup=pending`）；
  无关设备照常自检；恢复后重跑同一检查会先等旧读取收回，不与旧读取重叠。
- `range`：读数越界（`failed/out_of_range`）与读取自身报错（`error/read_error`）的区别，以及恢复。

## 结论语义（三类失败严格分开）

| 状态 | code | 含义 |
| --- | --- | --- |
| `error` | `sensor_missing` | 设备上不存在该传感器（未安装/地址不存在） |
| `error` | `read_error` / `check_panic` | 传感器存在，但读取或检查自身报错，检查无法完成 |
| `failed` | `out_of_range` | 通信正常、检查执行完成，但读数超出合格区间 |
| `timeout` | `timeout` | 检查超过单项时限（`Check.Timeout()`，默认 2s） |
| `canceled` | `canceled` | 外部 ctx 取消，检查未完成 |
| `blocked` | `upstream_not_passed` | 前置检查未通过，本项不得执行，**不计为通过** |
| `passed` | `ok` | 通过 |

依赖规则：检查项通过 `Deps()` 声明前置；任一前置不是 `passed`（包括重跑时前置尚无记录），本项直接记 `blocked`。即使上游是 `blocked`，错误链也会继续传递，不会出现“上游没完成、下游假装通过”。

## 执行与取消/超时

- `Executor.RunAll(ctx)` 按拓扑序执行整轮。
- 每个检查项使用独立的 `context.WithTimeout`；超时或取消与检查返回同时发生时，以 ctx 结论为准。
- 检查函数内 panic 会被兜底为 `error/check_panic`，不会炸掉整个执行器。
- 慢响应/取消要求检查函数尊重传入的 `ctx`；虚拟传感器在 `ctx.Done()` 后立即退出并释放句柄，`Device.Close()` 断言“在途读取=0、open==close”。

## 结论与资源收尾的边界

检查结论（`Status`）与设备资源收尾（`Record.Cleanup`）是两个独立维度：

- 超时/取消一旦落定，结论立即写入记录；但如果设备对取消无响应，检查 goroutine 可能仍在运行，
  此时记录标记 `Cleanup=pending`（在途），**不能视为完全结束**，`Report.FullySettled()` 为 false。
- `Executor.PendingCleanup()` 实时列出尚未收回的检查项；`Executor.WaitCleanup(ctx)` 阻塞等待全部收回。
  关闭设备、断言资源账本前应先调用 `WaitCleanup`。
- 超时/取消后检查 goroutine 的迟到结果只会进入缓冲通道并被丢弃，不会写回任何记录，不会污染后续自检。
- 重跑（`Rerun`）同一检查项时，执行器先等待该项的旧在途操作收回，再发起新读取，
  保证新旧读取绝不重叠（虚拟传感器句柄被并发占用会直接 panic，可用来验证这一点）。

## 重跑与历史

- `Executor.Affected(id)` 返回重跑范围：`id` 自身 + 依赖链上所有受影响下游（拓扑序）。
- `Executor.Rerun(ctx, id)` 只执行上述范围；范围外的既有结论原样保留（`Attempt` 不增加）。上游仍未通过时，下游重跑依旧是 `blocked`。
- 每次 `RunAll`/`Rerun` 都生成一个新的不可变 `Report` 追加到 `History`，旧批次不被改写。可通过 `History.Get(runID)`（从 1 开始）或 `History.Latest()` 取回。

## 代码结构

- `check/`：状态、记录、检查集（拓扑排序与环检测）、执行器、历史。
- `virtual/`：可控虚拟设备/传感器（缺失、越界、读取报错、慢延迟）及演示检查集。
- `cmd/devicecheck/`：可直接运行的场景化 CLI 演示。

## 在真实设备上接入

1. 实现 `check.Device`（`ID()`、`Model()`）。
2. 用 `check.FuncCheck`（或实现 `check.Check`）定义检查项，在 `Run` 内：传感器缺失 → `check.Fault("sensor_missing", ...)`；读取/检查自身失败 → `check.Fault("read_error", ...)`；越界 → `check.Fail("out_of_range", ...)`；正常 → `check.Pass(...)`，并尊重 `ctx` 的取消/超时。
3. `check.NewSet(model, checks...)` 构建并校验检查集，`check.NewExecutor(device, set, history)` 执行。
