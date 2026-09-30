// Command devicecheck 是设备自检执行器的实际运行入口。
//
// 子命令：+//
//
//	demo <missing|slow|recover>	运行内置虚拟设备场景
//	run 				整轮执行
//	rerun <check-id>...		重跑指定失败项及其受影响下游
//	history [run-id]		查看历史轮次（不给编号则列全部）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

const modelID = "VM-THERMO-1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "demo":
		err = runDemo(args)
	case "run":
		err = runOnce(args)
	case "rerun":
		err = runRerun(args)
	case "history":
		err = runHistory(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `设备自检执行器

用法:
  devicecheck demo <missing|slow|recover> [-json out.json]
  devicecheck run [-timeout 5s] [-json out.json]
  devicecheck rerun <check-id> [<check-id>...] [-json out.json]
  devicecheck history [<run-id>]

状态说明:
  passed   检查通过
  failed   读数越界等业务不达标（检查本身执行正常）
  error    检查无法完成（sensor_missing / sensor_fault / selftest_error 等）
  blocked  前置检查未通过，本项不执行（绝不计为通过）
  timeout  超过单项时限
  canceled 整轮被取消
`)
}

// stateFile 保存历史，使 run / rerun / history 可跨进程串成完整流程。
const stateFile = "devicecheck-state.json"

type persistentState struct {
	History []*check.Report `json:"history"`
}

func loadHistory() *check.History {
	h := &check.History{}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return h
	}
	var st persistentState
	if err := json.Unmarshal(data, &st); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 历史文件 %s 无法解析，按空历史启动: %v\n", stateFile, err)
		return h
	}
	for _, rep := range st.History {
		h.Save(rep)
	}
	return h
}

func saveHistory(h *check.History) error {
	st := persistentState{History: h.All()}
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0o644)
}

func parseJSONFlag(args []string) (rest []string, jsonOut string) {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	fs.StringVar(&jsonOut, "json", "", "将最新报告写入指定 JSON 文件")
	fs.Parse(args)
	return fs.Args(), jsonOut
}

func buildRig() (*virtual.Device, *check.CheckSet) {
	temp := virtual.NewSensor(42.5)
	fans := virtual.NewSensor(1800)
	dev := virtual.NewDevice("vm-device-01", modelID, map[string]*virtual.Sensor{
		"temp": temp,
		"fans": fans,
	})
	set, _ := virtual.NewSet(virtual.SetConfig{
		Model:   modelID,
		Timeout: 300 * time.Millisecond,
		Rules: []virtual.RangeRule{
			{Sensor: "temp", Min: 0, Max: 80},
			{Sensor: "fans", Min: 1000, Max: 2500},
		},
		Aggregates: map[string][]string{
			"thermal_summary": {"temp_range", "fans_range"},
		},
	})
	return dev, set
}

func finish(h *check.History, rep *check.Report, jsonOut string) error {
	if err := saveOnly(h, rep, jsonOut); err != nil {
		return err
	}
	printReport(rep)
	return nil
}

// saveOnly 持久化历史与可选 JSON，但不重复打印（demo 自己分阶段打印）。
func saveOnly(h *check.History, rep *check.Report, jsonOut string) error {
	if err := saveHistory(h); err != nil {
		return err
	}
	if jsonOut != "" {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonOut, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func runOnce(args []string) error {
	rest, jsonOut := parseJSONFlag(args)
	timeout := 30 * time.Second
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.DurationVar(&timeout, "timeout", timeout, "整轮超时")
	fs.Parse(rest)
	dev, set := buildRig()
	h := loadHistory()
	ex, err := check.NewExecutor(dev, set, h)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rep, err := ex.RunAll(ctx)
	if err != nil {
		return err
	}
	return finish(h, rep, jsonOut)
}

func runRerun(args []string) error {
	rest, jsonOut := parseJSONFlag(args)
	if len(rest) == 0 {
		return fmt.Errorf("rerun 至少需要一个检查项 ID")
	}
	h := loadHistory()
	if h.Len() == 0 {
		return fmt.Errorf("尚无历史轮次，请先执行 run")
	}
	dev, set := buildRig()
	ex, err := check.NewExecutor(dev, set, h)
	if err != nil {
		return err
	}
	// 用最近一轮记录恢复执行器当前结论，再在同一批虚拟设备状态上重跑。
	latest, _ := h.Latest()
	if err := ex.Restore(latest); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := ex.Rerun(ctx, rest...)
	if err != nil {
		return err
	}
	return finish(h, rep, jsonOut)
}

func runHistory(args []string) error {
	h := loadHistory()
	if len(args) == 0 {
		for _, rep := range h.All() {
			fmt.Printf("#%d overall=%s records=%d\n", rep.RunID, rep.Overall, len(rep.Records))
		}
		if h.Len() == 0 {
			fmt.Println("（暂无历史）")
		}
		return nil
	}
	var id int
	if _, err := fmt.Sscanf(args[0], "%d", &id); err != nil {
		return fmt.Errorf("轮次编号必须是整数: %q", args[0])
	}
	rep, ok := h.Get(id)
	if !ok {
		return fmt.Errorf("轮次 #%d 不存在", id)
	}
	printReport(rep)
	return nil
}

func printReport(rep *check.Report) {
	fmt.Printf("轮次 #%d 设备=%s 型号=%s 整体=%s 耗时=%s\n",
		rep.RunID, rep.DeviceID, rep.Model, rep.Overall, rep.EndedAt.Sub(rep.StartedAt).Round(time.Millisecond))
	for _, rec := range rep.Records {
		deps := ""
		if len(rec.Deps) > 0 {
			deps = fmt.Sprintf(" deps=%v", rec.Deps)
		}
		fmt.Printf("  [%d] %-18s %-9s code=%-20s%s\n        %s\n",
			rec.Attempt, rec.CheckID, rec.Status, rec.Code, deps, rec.Message)
	}
}
