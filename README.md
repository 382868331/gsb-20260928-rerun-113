# 设备自检执行器（devicecheck）

按依赖拓扑执行设备自检项，区分三类失败、处理取消/超时、支持“失败项 + 受影响下游”重跑，并保留完整历史批次。
自检过程中可按已完成检查的结论临时决定后续检查项（条件检查计划），
计划可查看、可保存重开，历史计划与新发现的检查项互不覆盖。

## 快速开始

```bash
go build ./...
go test -race ./...

# 可运行演示（虚拟设备），默认 all 依次演示全部场景
go run ./cmd/devicecheck [missing|slow|stuck|range|cond|all]
```

- `missing`：电源传感器不存在 → 安装恢复 → 只重跑 `power` 及其下游。
- `slow`：温度慢响应超时、100ms 后外部取消、恢复后只重跑 `temperature` 及下游，并校验资源收尾。
- `stuck`：温度读取对取消无响应 —— 超时结论先行给出并标记 `CleanupPending`，清理进展经 `InFlight` 单独展示；无关设备照常自检；同一设备重跑等待旧读取收尾，不与在途读取重叠。
- `range`：读数越界（`failed/out_of_range`）与读取自身报错（`error/read_error`）的区别，以及恢复。
- `cond`：条件检查计划 —— `temperature` 失败触发补充诊断（计划可查看每项加入原因）；
  修复后仅重测受影响部分；上游再次变化时指出哪些下游结论已过期；
  计划保存到 JSON 文件后重开，历史结论带原依据恢复，与新发现的检查项互不覆盖。

## 结论语义（三类失败严格分开）

| 状态 | code | 含义 |
| --- | --- | --- |
| `error` | `sensor_missing` | 设备上不存在该传感器（未安装/地址不存在） |
| `error` | `read_error` / `check_panic` | 传感器存在，但读取或检查自身报错，检查无法完成 |
| `failed` | `out_of_range` | 通信正常、检查执行完成，但读数超出合格区间 |
| `timeout` | `timeout` | 检查超过单项时限（`Check.Timeout()`，默认 2s） |
| `canceled` | `canceled` | 外部 ctx 取消，检查未完成 |
| `blocked` | `upstream_not_passed` | 前置检查未通过，本项不得执行，**不计为通过** |
| `pending` | `pending` | 已列入计划但尚未得出结论，**不得当成通过跳过** |
| `passed` | `ok` | 通过 |

## 条件检查计划（Plan）

- `Executor.Plan()` 返回可查看的计划：每项为何加入（`Reason`）、当前状态、已有结论。
  初始由检查集播种；`Executor.OnResult(rule...)` 注册条件分支规则，
  某条结论满足 `rule.When` 时把 `rule.Add()` 的补充检查项加入计划并尽量当轮执行。
- 计划只增不改：同一 ID 再次加入不覆盖已有条目与结论。
- 未得出结论的项在报告中记为 `pending`，整体结论不得为 `passed`；前置未执行的下游依旧 `blocked`。
- 每条结论记录其依据（`Record.Basis`：各前置当时的执行次数）。上游再次执行后，
  依据不再匹配的下游结论即为过期：本轮报告通过 `Report.Invalidated` 列出，
  当前仍过期的项可用 `Executor.Stale()` 查询后按需重测。
- `Plan.Save(w)` / `LoadPlan(r)` 把计划（含结论与依据）序列化为 JSON 后重开；
  `Executor.AdoptPlan(saved)` 把历史结论回填到新执行器（历史轮次经共享 `History` 接续），
  `Executor.Register(reason, checks...)` 在重开后恢复补充检查的定义。
  合并取并集：历史计划与后来新发现的检查项互不覆盖。

依赖规则：检查项通过 `Deps()` 声明前置；任一前置不是 `passed`（包括重跑时前置尚无记录），本项直接记 `blocked`。即使上游是 `blocked`，错误链也会继续传递，不会出现“上游没完成、下游假装通过”。

## 执行与取消/超时

- `Executor.RunAll(ctx)` 按拓扑序执行整轮。
- 每个检查项使用独立的 `context.WithTimeout`；超时或取消与检查返回同时发生时，以 ctx 结论为准。
- 检查函数内 panic 会被兜底为 `error/check_panic`，不会炸掉整个执行器。
- 慢响应/取消要求检查函数尊重传入的 `ctx`；虚拟传感器在 `ctx.Done()` 后立即退出并释放句柄，`Device.Close()` 断言“在途读取=0、open==close”。
- 超时/取消结论与资源收尾是两个阶段：若读取未随 ctx 退出（如无响应设备），记录会标记 `CleanupPending`，执行器通过 `InFlight()` 暴露清理进展、`WaitIdle(ctx)` 等待收尾；在此之前不得宣称资源已释放，`Device.Close()` 也会如实报错。
- 同一检查的前次读取尚未收回时，重跑会先等其收尾再发起新读取，新旧读取绝不重叠；迟到结果只清理自己的登记，不改写任何已有记录。虚拟传感器可用 `SetIgnoreContext(true)` 模拟取消无响应。

## 重跑与历史

- `Executor.Affected(id)` 返回重跑范围：`id` 自身 + 依赖链上所有受影响下游（拓扑序）。
- `Executor.Rerun(ctx, id)` 只执行上述范围；范围外的既有结论原样保留（`Attempt` 不增加）。上游仍未通过时，下游重跑依旧是 `blocked`。
- 每次 `RunAll`/`Rerun` 都生成一个新的不可变 `Report` 追加到 `History`，旧批次不被改写。可通过 `History.Get(runID)`（从 1 开始）或 `History.Latest()` 取回。

## 代码结构

- `check/`：状态、记录、检查集（拓扑排序与环检测）、执行器、条件计划、历史。
- `virtual/`：可控虚拟设备/传感器（缺失、越界、读取报错、慢延迟）及演示检查集。
- `cmd/devicecheck/`：可直接运行的场景化 CLI 演示。

## 在真实设备上接入

1. 实现 `check.Device`（`ID()`、`Model()`）。
2. 用 `check.FuncCheck`（或实现 `check.Check`）定义检查项，在 `Run` 内：传感器缺失 → `check.Fault("sensor_missing", ...)`；读取/检查自身失败 → `check.Fault("read_error", ...)`；越界 → `check.Fail("out_of_range", ...)`；正常 → `check.Pass(...)`，并尊重 `ctx` 的取消/超时。
3. `check.NewSet(model, checks...)` 构建并校验检查集，`check.NewExecutor(device, set, history)` 执行。
