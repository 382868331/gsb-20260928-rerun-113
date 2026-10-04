package check

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanLogAppendAndVersions(t *testing.T) {
	log := NewPlanLog([]string{"a", "b"})
	if log.Len() != 1 {
		t.Fatalf("初始应只有 1 个版本，得到 %d", log.Len())
	}
	cur := log.Current()
	if cur.Version != 1 || len(cur.Items) != 2 {
		t.Fatalf("初始计划版本/项数不对: %+v", cur)
	}
	for _, it := range cur.Items {
		if it.Reason != "初始检查集" || it.Since != 1 {
			t.Fatalf("初始项原因/版本不对: %+v", it)
		}
	}

	log.append([]PlanItem{{
		CheckID:  "c",
		Reason:   "b 失败，追加补充检查",
		Evidence: []Evidence{{CheckID: "b", Status: Failed, Code: "out_of_range", Message: "越界"}},
	}})
	if log.Len() != 2 {
		t.Fatalf("追加后应有 2 个版本，得到 %d", log.Len())
	}
	item, ok := log.Current().Item("c")
	if !ok || item.Since != 2 || item.Reason == "" {
		t.Fatalf("补充项未正确记录: %+v ok=%v", item, ok)
	}
	if len(item.Evidence) != 1 || item.Evidence[0].CheckID != "b" || item.Evidence[0].Status != Failed {
		t.Fatalf("补充项依据快照不对: %+v", item.Evidence)
	}

	// 历史版本不被后续调整改写。
	v1, ok := log.Get(1)
	if !ok || len(v1.Items) != 2 {
		t.Fatalf("版本 1 应保持 2 项，得到 %+v", v1)
	}
	if _, ok := v1.Item("c"); ok {
		t.Fatal("历史版本 1 不应包含后加的 c")
	}
}

func TestPlanLogSaveLoadAndDiff(t *testing.T) {
	log := NewPlanLog([]string{"a", "b"})
	log.append([]PlanItem{{
		CheckID:  "c",
		Reason:   "b 失败，追加补充检查",
		Evidence: []Evidence{{CheckID: "b", Status: Failed, Code: "out_of_range"}},
	}})
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := log.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadPlanLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Len() != 2 {
		t.Fatalf("重新打开后应有 2 个版本，得到 %d", loaded.Len())
	}

	// 保存并重新打开后，能解释当前计划与历史计划的差别。
	v1, _ := loaded.Get(1)
	diff := DiffPlans(v1, loaded.Current())
	if len(diff.Added) != 1 || diff.Added[0].CheckID != "c" {
		t.Fatalf("diff 应指出新增 c: %+v", diff)
	}
	if len(diff.Removed) != 0 {
		t.Fatalf("不应有移除项: %+v", diff)
	}
	text := diff.String()
	if !strings.Contains(text, "c") || !strings.Contains(text, "b 失败") || !strings.Contains(text, "out_of_range") {
		t.Fatalf("diff 说明应包含加入原因与当时依据:\n%s", text)
	}

	// 重新打开后仍可继续追加，版本号接续且不覆盖历史。
	loaded.append([]PlanItem{{CheckID: "d", Reason: "c 失败，继续追加"}})
	if loaded.Len() != 3 {
		t.Fatalf("继续追加后应有 3 个版本，得到 %d", loaded.Len())
	}
	v2, _ := loaded.Get(2)
	if len(v2.Items) != 3 {
		t.Fatalf("历史版本 2 不应被改写: %+v", v2)
	}
	diff2 := DiffPlans(v2, loaded.Current())
	if len(diff2.Added) != 1 || diff2.Added[0].CheckID != "d" {
		t.Fatalf("v2→v3 应只新增 d: %+v", diff2)
	}
}
