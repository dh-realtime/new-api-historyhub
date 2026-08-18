package historyhub

// 81 起要求的搜索全量测试矩阵(≥100 用例): 多天数据 + 新旧秒/毫秒混合 + 关键词范围/
// 大小写/日期 AND 组合/边界。835 起正则移除、标题退出匹配，新增 -排除 / A|B 或 /
// 问:答: 前缀 / 引号短语的矩阵。种子语料固定,期望结果逐一断言;任何回归在这里直接爆。
import (
	"sort"
	"strings"
	"testing"
	"time"
)

// ---- 种子语料: 2026-08-13 ~ 2026-08-15 (CST), 覆盖 ms/legacy-秒/混合/无msg/isKey ----

type seedMsg struct {
	q, a      string
	atMS      int64 // 毫秒
	legacySec bool  // true: 该行 q_at/a_at 直接写秒(历史遗留单位)
}

type seedSen struct {
	key    string // 语料名
	title  string
	isKey  int
	senSec int64 // sen.created_at/updated_at(秒)
	msgs   []seedMsg
}

func cst(y int, m time.Month, d, h, min int) int64 {
	return time.Date(y, m, d, h, min, 0, 0, time.Local).UnixMilli()
}

func searchCorpus() []seedSen {
	return []seedSen{
		{key: "S1", title: "标题含跟进度", senSec: cst(2026, 8, 13, 10, 0) / 1000,
			msgs: []seedMsg{{q: "普通问题", a: "普通回答", atMS: cst(2026, 8, 13, 10, 1)}}},
		{key: "S2", title: "八二三会话", senSec: cst(2026, 8, 14, 9, 0) / 1000,
			msgs: []seedMsg{{q: "化学实验怎么写", a: "先跟进度再分析化学式", atMS: cst(2026, 8, 14, 9, 1)}}},
		{key: "S3", title: "八二五会话", senSec: cst(2026, 8, 15, 9, 0) / 1000,
			msgs: []seedMsg{{q: "今天有事吗", a: "没什么", atMS: cst(2026, 8, 15, 9, 1)}}},
		{key: "S4", title: "跨天会话", senSec: cst(2026, 8, 14, 18, 0) / 1000,
			msgs: []seedMsg{
				{q: "跨天一问", a: "跨天一答", atMS: cst(2026, 8, 14, 18, 30)},
				{q: "跨天二问", a: "跨天二答", atMS: cst(2026, 8, 15, 8, 0)},
			}},
		{key: "S5", title: "旧秒级会话", senSec: cst(2026, 8, 14, 11, 0) / 1000,
			msgs: []seedMsg{{q: "旧秒级问题", a: "旧秒级回答", atMS: cst(2026, 8, 14, 11, 1), legacySec: true}}},
		{key: "S6", title: "混合单位会话", senSec: cst(2026, 8, 15, 7, 0) / 1000,
			msgs: []seedMsg{
				{q: "混合早问", a: "混合早答", atMS: cst(2026, 8, 15, 7, 30), legacySec: true},
				{q: "混合晚问", a: "混合晚答", atMS: cst(2026, 8, 15, 20, 0)},
			}},
		{key: "S7", title: "无消息会话", senSec: cst(2026, 8, 14, 12, 0) / 1000},
		{key: "S8", title: "密钥直连会话", isKey: 1, senSec: cst(2026, 8, 14, 13, 0) / 1000,
			msgs: []seedMsg{{q: "密钥直连问题", a: "密钥直连回答", atMS: cst(2026, 8, 14, 13, 1)}}},
		{key: "S9", title: "报错轮会话", senSec: cst(2026, 8, 15, 14, 0) / 1000,
			msgs: []seedMsg{{q: "报错提问", a: "[报错Error] 额度不够", atMS: cst(2026, 8, 15, 14, 1)}}},
		{key: "SA", title: "大小写语料", senSec: cst(2026, 8, 13, 16, 0) / 1000,
			msgs: []seedMsg{
				{q: "Println问题", a: "println答案", atMS: cst(2026, 8, 13, 16, 1)},
				{q: "CaSeMiXd问题", a: "mixed答案", atMS: cst(2026, 8, 13, 16, 5)},
			}},
		// SB 隔天会话(83 的真实形态，镜像生产 sen4)：13/15 两天有对话，14 号一轮都没有。
		// 旧"活动窗口重叠"判定会让它混进"仅14号"的结果 —— 逐轮判定后必须被排除。
		{key: "SB", title: "隔天会话", senSec: cst(2026, 8, 13, 20, 0) / 1000,
			msgs: []seedMsg{
				{q: "遗传信息问", a: "遗传信息答", atMS: cst(2026, 8, 13, 20, 1)},
				{q: "隔天二问", a: "隔天二答", atMS: cst(2026, 8, 15, 10, 0)},
			}},
		// 834/835 多条件语料：SC 同句双词(14号)、SD 只有音乐(14号)、SE 双词分在两轮(13/15号)、
		// SG 标题含音乐+正文只有媒介(15号) —— 835 起标题退出匹配，SG 是"标题词不参与"的回归语料。
		{key: "SC", title: "双词同句会话", senSec: cst(2026, 8, 14, 15, 0) / 1000,
			msgs: []seedMsg{{q: "音乐 媒介 都要", a: "回答里提到了媒介", atMS: cst(2026, 8, 14, 15, 1)}}},
		{key: "SD", title: "单词会话", senSec: cst(2026, 8, 14, 16, 0) / 1000,
			msgs: []seedMsg{{q: "音乐问题", a: "音乐答案", atMS: cst(2026, 8, 14, 16, 1)}}},
		{key: "SE", title: "分轮双词会话", senSec: cst(2026, 8, 13, 9, 0) / 1000,
			msgs: []seedMsg{
				{q: "音乐", a: "嗯", atMS: cst(2026, 8, 13, 9, 1)},
				{q: "看看", a: "媒介", atMS: cst(2026, 8, 15, 11, 0)},
			}},
		{key: "SG", title: "音乐课标题", senSec: cst(2026, 8, 15, 12, 0) / 1000,
			msgs: []seedMsg{{q: "别的", a: "媒介内容", atMS: cst(2026, 8, 15, 12, 1)}}},
		// 836 同轮同侧语料：SH 双词同答案(16号)、SI 双词分居同一轮的问题/答案(16号) ——
		// 836 起 AND 的命中单位是「同一轮的同一边」，SE/SI 这类跨轮/跨边分布不再命中。
		{key: "SH", title: "双词同答案会话", senSec: cst(2026, 8, 16, 10, 0) / 1000,
			msgs: []seedMsg{{q: "随便问", a: "这段答案同时提到音乐和媒介", atMS: cst(2026, 8, 16, 10, 1)}}},
		{key: "SI", title: "同轮跨边会话", senSec: cst(2026, 8, 16, 11, 0) / 1000,
			msgs: []seedMsg{{q: "音乐问题", a: "媒介答案", atMS: cst(2026, 8, 16, 11, 1)}}},
	}
}

var searchUID int64 = 990000

// seedSearchEnv 把 dbDir 指到临时目录,写入完整语料,返回(语料id表, 当前uid, 清理函数)。
func seedSearchEnv(t *testing.T) (map[string]int64, int, func()) {
	t.Helper()
	dbDirOverride = t.TempDir()
	uid := int(searchUID)
	searchUID++
	cleanup := func() { dbDirOverride = "" }

	byKey := map[string]int64{}
	d, err := shard.get(uid)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range searchCorpus() {
		sen := &Sen{TokenId: 1, IsKey: s.isKey, Title: s.title, CreatedAt: s.senSec, UpdatedAt: s.senSec}
		if err := d.Create(sen).Error; err != nil {
			t.Fatal(err)
		}
		byKey[s.key] = sen.Id
		for _, m := range s.msgs {
			qAt, aAt := m.atMS, m.atMS+1500
			if m.legacySec {
				qAt, aAt = m.atMS/1000, m.atMS/1000+1
			}
			row := &Msg{SenId: sen.Id, Q: m.q, A: m.a, QAt: qAt, AAt: aAt, TokensIn: 10, TokensOut: 20, TTFT: 100, TPS: 50}
			if err := d.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return byKey, uid, cleanup
}

func runSearch(t *testing.T, uid int, s senSearch) []int64 {
	t.Helper()
	rows, err := listSenRich(uid, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.Id)
	}
	return ids
}

func assertIDs(t *testing.T, got []int64, wantKeys []string, byKey map[string]int64) {
	t.Helper()
	want := make([]int64, 0, len(wantKeys))
	for _, k := range wantKeys {
		want = append(want, byKey[k])
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(got) != len(want) {
		t.Fatalf("got %v want %v (%s)", got, want, strings.Join(wantKeys, ","))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v (%s)", got, want, strings.Join(wantKeys, ","))
		}
	}
}

// ---- 主矩阵 ----

func TestSearchMatrix(t *testing.T) {
	byKey, uid, cleanup := seedSearchEnv(t)
	defer cleanup()

	d13, d14, d15 := "2026-08-13", "2026-08-14", "2026-08-15"
	mkDate := func(from, to string) senSearch {
		f, tt := parseDateRangeMS(from, to)
		return senSearch{dateFromMS: f, dateToMS: tt}
	}
	// qs: 按用户真实输入解析(覆盖 parseSearchTerms)，默认 问题+答案 范围(836: 命中单位=同轮同侧)
	qs := func(s string) senSearch { return senSearch{terms: parseSearchTerms(s), scopeQ: true, scopeA: true} }

	type tc struct {
		name string
		opts senSearch
		want []string
	}
	var cases []tc
	add := func(name string, opts senSearch, want ...string) {
		cases = append(cases, tc{name, opts, want})
	}

	// A. 关键词 × 范围 (18) —— 835 起标题退出匹配，标题词一律不命中
	add("A01 化学/问题", senSearch{keyword: "化学", scopeQ: true}, "S2")
	add("A02 化学/答案", senSearch{keyword: "化学", scopeA: true}, "S2")
	add("A03 化学/无范围(store层)=空", senSearch{keyword: "化学"})
	add("A04 化学/问题+答案", senSearch{keyword: "化学", scopeQ: true, scopeA: true}, "S2")
	add("A05 跟进度/答案(标题也含但不再参与)", senSearch{keyword: "跟进度", scopeA: true}, "S2")
	add("A06 跟进度/问题=空(词只在答案)", senSearch{keyword: "跟进度", scopeQ: true})
	add("A07 标题词「标题含」不参与=空", senSearch{keyword: "标题含", scopeQ: true, scopeA: true})
	add("A08 普通回答/答案", senSearch{keyword: "普通回答", scopeA: true}, "S1")
	add("A09 密钥直连/问题/含isKey", senSearch{keyword: "密钥直连", scopeQ: true, includeKey: true}, "S8")
	add("A10 密钥直连/默认排除isKey=空", senSearch{keyword: "密钥直连", scopeQ: true})
	add("A11 额度不够/答案", senSearch{keyword: "额度不够", scopeA: true}, "S9")
	add("A12 报错前缀方括号字面匹配(无正则转义)", senSearch{keyword: "[报错Error]", scopeA: true}, "S9")
	add("A13 空关键词=全量(不含isKey)", senSearch{}, "S1", "S2", "S3", "S4", "S5", "S6", "S7", "S9", "SA", "SB", "SC", "SD", "SE", "SG", "SH", "SI")
	add("A14 空关键词+isKey=全量", senSearch{includeKey: true}, "S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8", "S9", "SA", "SB", "SC", "SD", "SE", "SG", "SH", "SI")
	add("A15 不存在的词=空", senSearch{keyword: "不存在词", scopeQ: true, scopeA: true})
	add("A16 百分号按字面%", senSearch{keyword: "%", scopeQ: true, scopeA: true})
	add("A17 下划线按字面_", senSearch{keyword: "_", scopeQ: true, scopeA: true})
	add("A18 标题词「八二三」全范围=空", senSearch{keyword: "八二三", scopeQ: true, scopeA: true})

	// B. 大小写开关 (12) —— 835 起默认不区分(caseSense 零值=false)
	add("B01 Println敏感/问题", senSearch{keyword: "Println", scopeQ: true, caseSense: true}, "SA")
	add("B02 println敏感/问题=空", senSearch{keyword: "println", scopeQ: true, caseSense: true})
	add("B03 println不敏感/问题", senSearch{keyword: "println", scopeQ: true}, "SA")
	add("B04 Println不敏感/问题", senSearch{keyword: "Println", scopeQ: true, caseSense: false}, "SA")
	add("B05 println敏感/答案", senSearch{keyword: "println", scopeA: true, caseSense: true}, "SA")
	add("B06 PRINTLN不敏感/答案", senSearch{keyword: "PRINTLN", scopeA: true, caseSense: false}, "SA")
	add("B07 PRINTLN敏感/答案=空", senSearch{keyword: "PRINTLN", scopeA: true, caseSense: true})
	add("B08 CaSeMiXd敏感", senSearch{keyword: "CaSeMiXd", scopeQ: true, caseSense: true}, "SA")
	add("B09 casemixd敏感=空", senSearch{keyword: "casemixd", scopeQ: true, caseSense: true})
	add("B10 casemixd不敏感", senSearch{keyword: "casemixd", scopeQ: true, caseSense: false}, "SA")
	add("B11 默认即不敏感(不设caseSense)", senSearch{keyword: "PRINTLN", scopeA: true}, "SA")
	add("B12 不敏感对中文无影响", senSearch{keyword: "遗传信息", scopeQ: true, scopeA: true}, "SB")

	// D. 日期范围 (30) —— 83 起日期按"逐轮对话"判定: 该轮 q_at/a_at 落在区间内才算,
	// 活动窗口跨过区间但当天一轮都没有的会话(如 SB: 13/15 有、14 无)不得出现。
	d14set := []string{"S2", "S4", "S5", "S7", "SC", "SD"}
	d15set := []string{"S3", "S4", "S6", "S9", "SB", "SE", "SG"}
	d13set := []string{"S1", "SA", "SB", "SE"}
	add("D01 单日13", mkDate(d13, d13), d13set...)
	add("D02 单日14", mkDate(d14, d14), d14set...)
	add("D03 单日14+isKey", func() senSearch { x := mkDate(d14, d14); x.includeKey = true; return x }(), "S2", "S4", "S5", "S7", "S8", "SC", "SD")
	add("D04 单日15", mkDate(d15, d15), d15set...)
	add("D05 全程13~15", mkDate(d13, d15), "S1", "S2", "S3", "S4", "S5", "S6", "S7", "S9", "SA", "SB", "SC", "SD", "SE", "SG")
	add("D06 仅from=15", mkDate(d15, ""), "S3", "S4", "S6", "S9", "SB", "SE", "SG", "SH", "SI")
	add("D07 仅to=13", mkDate("", d13), d13set...)
	add("D08 仅to=14", mkDate("", d14), "S1", "S2", "S4", "S5", "S7", "SA", "SB", "SC", "SD", "SE")
	add("D09 仅from=14", mkDate(d14, ""), "S2", "S3", "S4", "S5", "S6", "S7", "S9", "SB", "SC", "SD", "SE", "SG", "SH", "SI")
	add("D10 反转15~14=空", mkDate(d15, d14))
	add("D11 上月=空", mkDate("2026-07-01", "2026-07-31"))
	add("D12 下月=空", mkDate("2026-09-01", "2026-09-30"))
	add("D13 整月8月=全量", mkDate("2026-08-01", "2026-08-31"), "S1", "S2", "S3", "S4", "S5", "S6", "S7", "S9", "SA", "SB", "SC", "SD", "SE", "SG", "SH", "SI")
	add("D14 双空=不过滤", mkDate("", ""), "S1", "S2", "S3", "S4", "S5", "S6", "S7", "S9", "SA", "SB", "SC", "SD", "SE", "SG", "SH", "SI")
	add("D15 13~14", mkDate(d13, d14), "S1", "S2", "S4", "S5", "S7", "SA", "SB", "SC", "SD", "SE")
	add("D16 14~15", mkDate(d14, d15), "S2", "S3", "S4", "S5", "S6", "S7", "S9", "SB", "SC", "SD", "SE", "SG")
	add("D17 跨天会话被14命中(14号真有对话)", mkDate(d14, d14), d14set...)
	add("D18 跨天会话被15命中(15号真有对话)", mkDate(d15, d15), d15set...)
	add("D19 跨天会话被13排除", mkDate(d13, d13), d13set...)
	add("D20 旧秒级(14)命中", mkDate(d14, d14), d14set...)
	add("D21 旧秒级被13排除", mkDate(d13, d13), d13set...)
	add("D22 混合单位(15)命中", mkDate(d15, d15), d15set...)
	add("D23 混合单位被14排除", mkDate(d14, d14), d14set...)
	add("D24 无消息会话回退sen时间(14)", mkDate(d14, d14), d14set...)
	add("D25 无消息会话被15排除", mkDate(d15, d15), d15set...)
	add("D26 毫秒级边界=单瞬含S2", senSearch{dateFromMS: cst(2026, 8, 14, 9, 1), dateToMS: cst(2026, 8, 14, 9, 1)}, "S2")
	add("D27 隔天会话(13/15)被14排除=83修复", mkDate(d14, d14), d14set...)
	add("D28 隔天会话被13命中", mkDate(d13, d13), d13set...)
	add("D29 隔天会话被15命中", mkDate(d15, d15), d15set...)
	add("D30 隔天会话毫秒边界(13日20:01整瞬)", senSearch{dateFromMS: cst(2026, 8, 13, 20, 1), dateToMS: cst(2026, 8, 13, 20, 1)}, "SB")

	// E. 日期 AND 关键词 (18) —— 用户实测场景(835 起全部为普通文本词)
	add("E01 用户场景:化学+全范围+单日14", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "化学", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}(), "S2")
	add("E02 用户场景+单日15=空", func() senSearch {
		f, tt := parseDateRangeMS(d15, d15)
		return senSearch{keyword: "化学", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E03 14×跨天", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "跨天", scopeQ: true, dateFromMS: f, dateToMS: tt}
	}(), "S4")
	add("E04 13×跨天=空", func() senSearch {
		f, tt := parseDateRangeMS(d13, d13)
		return senSearch{keyword: "跨天", scopeQ: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E05 15×没什么", func() senSearch {
		f, tt := parseDateRangeMS(d15, d15)
		return senSearch{keyword: "没什么", scopeA: true, dateFromMS: f, dateToMS: tt}
	}(), "S3")
	add("E06 14×没什么=空", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "没什么", scopeA: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E07 from13×println不敏感", senSearch{keyword: "println", scopeA: true, caseSense: false, dateFromMS: cst(2026, 8, 13, 0, 0)}, "SA")
	add("E08 14×旧秒级", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "旧秒级", scopeQ: true, dateFromMS: f, dateToMS: tt}
	}(), "S5")
	add("E09 13×旧秒级=空", func() senSearch {
		f, tt := parseDateRangeMS(d13, d13)
		return senSearch{keyword: "旧秒级", scopeQ: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E10 15×混合", func() senSearch {
		f, tt := parseDateRangeMS(d15, d15)
		return senSearch{keyword: "混合", scopeQ: true, dateFromMS: f, dateToMS: tt}
	}(), "S6")
	add("E11 14×密钥+isKey", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "密钥", scopeQ: true, includeKey: true, dateFromMS: f, dateToMS: tt}
	}(), "S8")
	add("E12 15×报错", func() senSearch {
		f, tt := parseDateRangeMS(d15, d15)
		return senSearch{keyword: "报错", scopeQ: true, dateFromMS: f, dateToMS: tt}
	}(), "S9")
	add("E13 14×标题词八二三=空(标题不参与)", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "八二三", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E14 13×八二三=空", func() senSearch {
		f, tt := parseDateRangeMS(d13, d13)
		return senSearch{keyword: "八二三", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E15 用户83场景:遗传信息×仅14=空(词在13轮,14无对话)", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "遗传信息", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}())
	add("E16 遗传信息×仅13", func() senSearch {
		f, tt := parseDateRangeMS(d13, d13)
		return senSearch{keyword: "遗传信息", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}(), "SB")
	add("E17 遗传信息×仅15(会话级AND:词在13轮+15轮有对话)", func() senSearch {
		f, tt := parseDateRangeMS(d15, d15)
		return senSearch{keyword: "遗传信息", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}(), "SB")
	add("E18 遗传信息×无日期", senSearch{keyword: "遗传信息", scopeQ: true, scopeA: true}, "SB")

	// G. 多条件 AND / 引号短语 (14) —— 836 起命中单位=「同一轮的同一边」：
	// 所有普通词须同时出现在同一轮的问题或答案文本里；跨轮(SE)/跨边(SI)分布不再命中。
	add("G01 音乐+媒介 AND(同轮同侧:SC问题边/SH答案边)", qs("音乐 媒介"), "SC", "SH")
	add("G02 仅音乐(标题音乐课不再命中)", qs("音乐"), "SC", "SD", "SE", "SH", "SI")
	add("G03 仅媒介", qs("媒介"), "SC", "SE", "SG", "SH", "SI")
	add("G04 引号短语=单条件含空格", senSearch{terms: parseSearchTerms(`"音乐 媒介"`), scopeQ: true}, "SC")
	add("G05 AND+OR同轮同侧(跨天与一答/二答同在答案边)", qs("跨天 一答|二答"), "S4")
	add("G06 标题词AND正文词=空(音乐课只在标题)", qs("音乐课 媒介"))
	add("G07 AND+日期仅14", func() senSearch {
		x := qs("音乐 媒介")
		f, t := parseDateRangeMS(d14, d14)
		x.dateFromMS, x.dateToMS = f, t
		return x
	}(), "SC")
	add("G08 范围全关=空(标题退出后无可命中字段)", senSearch{terms: parseSearchTerms("音乐 媒介")})
	add("G09 大小写敏感双条件跨字段=不再命中", senSearch{terms: parseSearchTerms("Println println"), scopeQ: true, scopeA: true, caseSense: true})
	add("G10 三条件AND(同侧凑齐)", qs("音乐 媒介 都要"), "SC")
	add("G11 AND含不存在词=空", qs("音乐 不存在的词"))
	add("G12 引号内|为字面字符", qs(`"音乐|媒介"`))
	add("G13 短语只搜答案=空", senSearch{terms: parseSearchTerms(`"音乐 媒介"`), scopeA: true})
	add("G14 AND+日期仅15(双词分在13/15两轮=不再命中)", func() senSearch {
		x := qs("音乐 媒介")
		f, t := parseDateRangeMS(d15, d15)
		x.dateFromMS, x.dateToMS = f, t
		return x
	}())

	// N. 排除词 - (9) —— 836 起按轮判定：该轮出现排除词只淘汰该轮，
	// 会话只要有任意一轮「含全部正词且不含排除词」就命中。
	add("N01 音乐 -媒介(SE一轮干净即命中,另一轮有媒介不影响)", qs("音乐 -媒介"), "SD", "SE")
	add("N02 仅排除-音乐(存在无该词的轮即保留;无消息会话不再命中)", qs("-音乐"), "S1", "S2", "S3", "S4", "S5", "S6", "S9", "SA", "SB", "SE", "SG")
	add("N03 报错 -不存在词", qs("报错 -不存在词"), "S9")
	add("N04 报错 -额度=空(唯一轮的答案含排除词)", qs("报错 -额度"))
	add("N05 -\"音乐 媒介\" 排除整词短语(按轮)", qs(`-"音乐 媒介"`), "S1", "S2", "S3", "S4", "S5", "S6", "S9", "SA", "SB", "SD", "SE", "SG", "SH", "SI")
	add("N06 排除词×仅答案范围", senSearch{terms: parseSearchTerms("-媒介"), scopeA: true}, "S1", "S2", "S3", "S4", "S5", "S6", "S9", "SA", "SB", "SD", "SE")
	add("N07 排除按轮判定(二问在15轮,不影响14轮命中)", func() senSearch {
		x := qs("跨天 -二问")
		f, t := parseDateRangeMS(d14, d14)
		x.dateFromMS, x.dateToMS = f, t
		return x
	}(), "S4")
	add("N08 排除+OR候选(音乐 AND 非都要|看看)", qs("音乐 -都要|看看"), "SD", "SE", "SH", "SI")
	add("N09 -密钥直连×14日+isKey", func() senSearch {
		x := senSearch{terms: parseSearchTerms("-密钥直连"), scopeQ: true, scopeA: true, includeKey: true}
		f, t := parseDateRangeMS(d14, d14)
		x.dateFromMS, x.dateToMS = f, t
		return x
	}(), "S2", "S4", "S5", "SC", "SD")

	// O. 或者 | (6) —— 裸词内竖线分隔候选，任一命中即满足(单词条件，语义与轮级无冲突)
	add("O01 音乐|媒介", qs("音乐|媒介"), "SC", "SD", "SE", "SG", "SH", "SI")
	add("O02 多候选中英混合不敏感", qs("化学式|跨天|println"), "S2", "S4", "SA")
	add("O03 OR×仅问题范围", senSearch{terms: parseSearchTerms("媒介|跨天"), scopeQ: true}, "S4", "SC")
	add("O04 OR+敏感", senSearch{terms: parseSearchTerms("Println|CaSeMiXd"), scopeQ: true, caseSense: true}, "SA")
	add("O05 OR+敏感=空(大小写不符)", senSearch{terms: parseSearchTerms("println|casemixd"), scopeQ: true, caseSense: true})
	add("O06 空候选被过滤(音乐||媒介|)", qs("音乐||媒介|"), "SC", "SD", "SE", "SG", "SH", "SI")

	// P. 问:/答: 前缀 (10) —— 单个词限定字段，优先于范围 chips
	add("P01 问:报错", qs("问:报错"), "S9")
	add("P02 答:报错", qs("答:报错"), "S9")
	add("P03 问:音乐", qs("问:音乐"), "SC", "SD", "SE", "SI")
	add("P04 答:音乐", qs("答:音乐"), "SD", "SH")
	add("P05 答:媒介", qs("答:媒介"), "SC", "SE", "SG", "SH", "SI")
	add("P06 问:Println+敏感", senSearch{terms: parseSearchTerms("问:Println"), caseSense: true}, "SA")
	add("P07 问:混合|旧秒级(前缀+OR)", qs("问:混合|旧秒级"), "S5", "S6")
	add("P08 -问:报错×15日", func() senSearch {
		x := qs("-问:报错")
		f, t := parseDateRangeMS(d15, d15)
		x.dateFromMS, x.dateToMS = f, t
		return x
	}(), "S3", "S4", "S6", "SB", "SE", "SG")
	add("P09 问:\"音乐 媒介\"(前缀+引号短语)", qs(`问:"音乐 媒介"`), "SC")
	add("P10 前缀在范围chips全关时仍生效", senSearch{terms: parseSearchTerms("问:音乐")}, "SC", "SD", "SE", "SI")

	// H. 其它角度 (10)
	add("H01 整条问题原文", senSearch{keyword: "化学实验怎么写", scopeQ: true}, "S2")
	add("H02 同msg问题答案都含词只算一次", senSearch{keyword: "旧秒级", scopeQ: true, scopeA: true}, "S5")
	add("H03 跨字段不拼接(问题尾+答案头)", senSearch{keyword: "怎么写先", scopeQ: true, scopeA: true})
	add("H04 日期全程×密钥(默认排除isKey)", func() senSearch {
		x := mkDate(d13, d15)
		x.keyword, x.scopeQ = "密钥", true
		return x
	}())
	add("H05 数字不搜tokens列", senSearch{keyword: "50", scopeQ: true, scopeA: true})
	add("H06 英文子串敏感", senSearch{keyword: "rint", scopeQ: true, caseSense: true}, "SA")
	add("H07 中文标点按字面", senSearch{keyword: "[", scopeA: true}, "S9")
	add("H08 全角空格同样分隔AND(同轮同侧)", qs("音乐　媒介"), "SC", "SH")
	add("H09 标题词「会话」×14=空(标题不参与)", func() senSearch {
		f, tt := parseDateRangeMS(d14, d14)
		return senSearch{keyword: "会话", scopeQ: true, scopeA: true, dateFromMS: f, dateToMS: tt}
	}())
	add("H10 长关键词(整句答案)", senSearch{keyword: "先跟进度再分析化学式", scopeA: true}, "S2")

	// T. 836 同轮同侧语义 (9) —— 命中单位=单轮单边；显式前缀固定边；排除按轮
	add("T01 仅答案范围+双词同答案命中", senSearch{terms: parseSearchTerms("音乐 媒介"), scopeA: true}, "SH")
	add("T02 仅问题范围+双词同问题命中", senSearch{terms: parseSearchTerms("音乐 媒介"), scopeQ: true}, "SC")
	add("T03 同轮跨边不命中(SI:音乐问题在问/媒介答案在答)", qs("音乐问题 媒介答案"))
	add("T04 问:音乐 答:媒介(显式前缀跨边同轮)命中SC/SI", qs("问:音乐 答:媒介"), "SC", "SI")
	add("T05 显式前缀+普通词同侧凑齐", qs("问:报错 提问"), "S9")
	add("T06 显式前缀与普通词可分居两边(同轮即可)", qs("问:报错 额度"), "S9")
	add("T07 keyword回退:整体含空格按单字面词", senSearch{keyword: "音乐 媒介", scopeQ: true, scopeA: true}, "SC")
	add("T08 引号短语×全范围(单条件)", senSearch{terms: parseSearchTerms(`"音乐 媒介"`), scopeQ: true, scopeA: true}, "SC")
	add("T09 双排除词(音乐 -媒介 -都要)", qs("音乐 -媒介 -都要"), "SD", "SE")

	total := 0
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assertIDs(t, runSearch(t, uid, c.opts), c.want, byKey)
		})
		total++
	}

	// 聚合正确性 (5): msg_count / first / last, 覆盖跨天/旧秒/混合/无msg
	agg := []struct {
		key       string
		cnt       int64
		first, la int64
	}{
		{"S4", 2, cst(2026, 8, 14, 18, 30), cst(2026, 8, 15, 8, 0) + 1500},
		{"S5", 1, cst(2026, 8, 14, 11, 1), cst(2026, 8, 14, 11, 1) + 1000},
		{"S6", 2, cst(2026, 8, 15, 7, 30), cst(2026, 8, 15, 20, 0) + 1500},
		{"S7", 0, cst(2026, 8, 14, 12, 0), cst(2026, 8, 14, 12, 0)},
		{"SB", 2, cst(2026, 8, 13, 20, 1), cst(2026, 8, 15, 10, 0) + 1500},
	}
	for _, a := range agg {
		a := a
		t.Run("F-agg-"+a.key, func(t *testing.T) {
			rows, err := listSenRich(uid, senSearch{})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Id != byKey[a.key] {
					continue
				}
				if r.MsgCount != a.cnt {
					t.Fatalf("msg_count got %d want %d", r.MsgCount, a.cnt)
				}
				if toMS(r.FirstAt) != a.first {
					t.Fatalf("first_at got %d want %d", r.FirstAt, a.first)
				}
				if toMS(r.LastAt) != a.la {
					t.Fatalf("last_at got %d want %d", r.LastAt, a.la)
				}
				return
			}
			t.Fatal("sen not found")
		})
		total++
	}

	t.Logf("共 %d 个搜索用例", total)
	if total < 100 {
		t.Fatalf("用例数 %d < 100", total)
	}
}

// ---- parseDateRangeMS 单测 (10) ----

func TestParseDateRangeMS(t *testing.T) {
	cases := []struct {
		name             string
		from, to         string
		wantFrom, wantTo int64
	}{
		{"正常14", "2026-08-14", "2026-08-14", cst(2026, 8, 14, 0, 0), cst(2026, 8, 15, 0, 0) - 1},
		{"跨月", "2026-07-31", "2026-08-01", cst(2026, 7, 31, 0, 0), cst(2026, 8, 2, 0, 0) - 1},
		{"闰年", "2024-02-29", "2024-02-29", cst(2024, 2, 29, 0, 0), cst(2024, 3, 1, 0, 0) - 1},
		{"空from", "", "2026-08-14", 0, cst(2026, 8, 15, 0, 0) - 1},
		{"空to", "2026-08-14", "", cst(2026, 8, 14, 0, 0), 0},
		{"双空", "", "", 0, 0},
		{"斜杠格式拒绝", "2026/08/14", "2026/08/14", 0, 0},
		{"非法日", "2026-02-30", "", 0, 0},
		{"带空格trim", " 2026-08-14 ", " 2026-08-15 ", cst(2026, 8, 14, 0, 0), cst(2026, 8, 16, 0, 0) - 1},
		{"反转(由过滤逻辑判空)", "2026-08-15", "2026-08-14", cst(2026, 8, 15, 0, 0), cst(2026, 8, 15, 0, 0) - 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f, tt := parseDateRangeMS(c.from, c.to)
			if f != c.wantFrom || tt != c.wantTo {
				t.Fatalf("got (%d,%d) want (%d,%d)", f, tt, c.wantFrom, c.wantTo)
			}
		})
	}
}

// ---- parseSearchTerms 单测 (834/835) ----
// 期望串编码: 条件以逗号相连; 条件内 [-][问:|答:]后接候选,候选以 ∥ 相连
// (引号短语只有一个候选、内部 | 为字面;裸词的 | 拆成多个候选)。

func fmtTerms(ts []searchTerm) string {
	var out []string
	for _, t := range ts {
		s := ""
		if t.negate {
			s += "-"
		}
		switch t.field {
		case 'q':
			s += "问:"
		case 'a':
			s += "答:"
		}
		s += strings.Join(t.alts, "∥")
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func TestParseSearchTerms(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"音乐", "音乐"},
		{"音乐 媒介", "音乐,媒介"},
		{"音乐  媒介\t都要", "音乐,媒介,都要"},
		{"音乐　媒介", "音乐,媒介"},         // 全角空格
		{`"音乐 媒介"`, "音乐 媒介"},       // 引号短语=单条件
		{`a "b c" d`, "a,b c,d"},   // 混合
		{`音乐 "媒介 都要"`, "音乐,媒介 都要"}, // 短语在末尾
		{`音乐"`, "音乐"},              // 未闭合引号
		{`"音乐`, "音乐"},              // 只有开引号
		{`""`, ""},                 // 空引号
		{`a"b`, "a,b"},             // 引号兼作分隔
		{"  音乐   媒介  ", "音乐,媒介"},   // 首尾空白
		{"-音乐", "-音乐"},             // 排除词
		{"音乐 -媒介", "音乐,-媒介"},       // AND+排除
		{"音乐 -媒介 都要", "音乐,-媒介,都要"}, // 三条件含排除
		{"-问:报错", "-问:报错"},         // 排除+字段
		{"问:报错 答:额度", "问:报错,答:额度"}, // 双字段
		{"音乐|媒介", "音乐∥媒介"},         // OR 拆候选
		{"音乐||媒介|", "音乐∥媒介"},       // 空候选过滤
		{`"音乐|媒介"`, "音乐|媒介"},       // 引号内|字面(单候选)
		{`问:"报错 信息"`, "问:报错 信息"},   // 字段+引号短语
		{"-", ""},            // 裸 - 丢弃
		{"问:", ""},           // 前缀无内容丢弃
		{"音乐 - 媒介", "音乐,媒介"}, // 孤立-丢弃(仍为AND)
		{"CO-2", "CO-2"},     // 词中-为字面
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			if got := fmtTerms(parseSearchTerms(c.in)); got != c.want {
				t.Fatalf("parse %q got %q want %q", c.in, got, c.want)
			}
		})
	}
}
