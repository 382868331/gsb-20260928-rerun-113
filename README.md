# 设备自检执行器（devicecheck）

按依赖拓扑执行设备自检项，支持单项超时、整轮取消、失败项及受影响下游的选择性重跑，
并把每一轮作为不可变批次保存在历史中。附带可运行的虚拟设备与命令行入口，直接演示
“传感器缺失 / 读数越界 / 检查自身报错”三类问题、慢响应超时、取消收尾和故障恢复。

## 构建与测试

    go build ./...
    go test ./...            # 功能测试
    go test -race ./...      # 并发与资源收尾（超时、取消场景）
    go build -o devicecheck ./cmd/devicecheck

## 快速演示

    ./devicecheck demo missing  # 传感器缺失 + 另一路读数越界，恢复后多选重跑
    ./devicecheck demo slow     # 慢响应：单项超时、整轮取消（验证在途读取收尾），再恢复重跑
    ./devicecheck demo recover  # 读数越界与自检逻辑报错分别演示，上游不修复时下游不放行

## 实际运行流程

历史保存在当前目录 devicecheck-state.json，因此 run / rerun / history 可跨进程串联：

    ./devicecheck run                            # 整轮执行，落盘为一轮历史
    ./devicecheck history                        # 列出历轮
    ./devicecheck history 1                      # 查看第 1 轮完整明细
    ./devicecheck rerun temp_range               # 只重跑 temp_range 及其受影响下游
    ./devicecheck rerun temp_range fans_range    # 一次选择多个失败项
    ./devicecheck run -json report.json          # 同时导出本轮报告 JSON

注意：普通 run/rerun 命令使用健康的演示设备；缺失、慢响应、故障注入等现场场景通过
demo 子命令在同一进程内完成“故障 → 恢复 → 重跑”的闭环演示。

## 结论语义

三类问题严格分开，禁止混为“一个失败”：

- failed / out_of_range：通信、检查正常，读数越界等业务不达标。
- error / sensor_missing：传感器不存在，检查无法完成。
- error / sensor_fault：传感器或读取链路报错。
- error / selftest_error：检查或设备自检逻辑自身报错。
- error / check_panic：检查函数 panic，被执行器兜底捕获。
- blocked / upstream_not_passed：前置存在但未通过，本项不执行。
- blocked / upstream_not_run：前置尚无记录（例如只重跑下游），同样不放行。
- timeout / timeout：超过单项时限。
- canceled / canceled：整轮被外部取消。

关键规则：上游未通过时下游一律 blocked，绝不会假装通过；整体结论按严重度聚合
（error > canceled > timeout > failed > blocked/stale > passed）。

## 重跑与历史

- Rerun(ctx, checkID...) 接收一个或多个失败项，实际执行集合是这些项的并集加上依赖链
  可达的全部下游，仍按拓扑序执行。
- 不在重跑集合内的检查项保留上一轮记录（报告中 Attempt 不增加）。
- 每次执行（整轮或重跑）都向历史追加一份新的不可变 Report，旧批次永不被改写。
- 需要跨进程重跑时，Executor.Restore(latestReport) 可从历史报告恢复当前结论。

## 取消、超时与资源收尾

- 每个检查项有独立的超时 context；父 context 取消时区分 timeout 与 canceled。
- 检查函数必须监听传入的 ctx；虚拟传感器的慢读取同样响应取消。
- 超时/取消后执行器等待检查 goroutine 退出后才结束本轮（WaitGroup），慢结果不会回写到
  已结束的报告。虚拟设备的 WaitIdle() 可等待全部在途读取退出，测试用它验证 goroutine 与
  设备句柄不泄漏（go test -race ./...）。

## 包结构

- check：核心执行器。Check、CheckSet（拓扑排序/环检测）、Executor、Report、History。
- virtual：虚拟设备、可配置传感器（缺失/故障/慢响应/恢复）与检查集构造器。
- cmd/devicecheck：实际执行入口（demo / run / rerun / history）。

## 在自己的型号上接入

1. 实现 check.Device（ID()、Model()）。
2. 用 check.FuncCheck（或实现 check.Check）声明检查项、依赖与超时；读数越界返回
   check.Fail("out_of_range", ...)，传感器/检查无法完成返回 check.Fault(code, ...)，
   读取过程响应 ctx.Done()。
3. check.NewSet(model, checks...) 校验重复 ID、缺失依赖和依赖环。
4. check.NewExecutor(dev, set, history) 后调用 RunAll / Rerun，从返回的 *Report 和
   History 读取结果。
