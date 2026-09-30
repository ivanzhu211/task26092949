package gatherup

import (
	"reflect"
	"testing"
)

// regByID 在快照中按编号查找报名。
func regByID(t *testing.T, v View, id int) RegistrationView {
	t.Helper()
	for _, r := range v.Registrations {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("registration %d not found in view", id)
	return RegistrationView{}
}

// checkInvariant 校验：占位不超容量，且不存在能放下却未分配的候补。
func checkInvariant(t *testing.T, a *Activity, now int64) {
	t.Helper()
	v := a.View(now)
	if v.Remaining < 0 || v.Remaining > v.Capacity {
		t.Fatalf("t=%d: remaining %d out of range [0,%d]", now, v.Remaining, v.Capacity)
	}
	for _, r := range v.Registrations {
		if r.Status == StatusWaitlisted && r.Size <= v.Remaining {
			t.Fatalf("t=%d: waitlisted reg %d (size %d) fits in remaining %d but was not allocated",
				now, r.ID, r.Size, v.Remaining)
		}
	}
}

// TestSameTickExpiryGroupStaysTogether 同刻到期的 2 人组和 1 人组共释放 3 席时，
// 最早的 3 人候补应整组获席，不能先把 2 席分给较晚的 2 人候补。
func TestSameTickExpiryGroupStaysTogether(t *testing.T) {
	a := NewActivity(3)

	idA, err := a.Register(0, 2) // 占 2 席，t=900 到期
	if err != nil {
		t.Fatal(err)
	}
	idB, err := a.Register(0, 1) // 占 1 席，t=900 到期
	if err != nil {
		t.Fatal(err)
	}
	idC, err := a.Register(0, 3) // 候补：3 人组
	if err != nil {
		t.Fatal(err)
	}
	idD, err := a.Register(0, 2) // 候补：2 人组，比 C 晚
	if err != nil {
		t.Fatal(err)
	}

	v := a.View(0)
	if v.Remaining != 0 {
		t.Fatalf("t=0: remaining = %d, want 0", v.Remaining)
	}

	v = a.View(900) // A、B 同刻到期，共释放 3 席
	if got := regByID(t, v, idA); got.Status != StatusCancelled || got.CancelledAt != 900 {
		t.Fatalf("A = %+v, want cancelled at 900", got)
	}
	if got := regByID(t, v, idB); got.Status != StatusCancelled || got.CancelledAt != 900 {
		t.Fatalf("B = %+v, want cancelled at 900", got)
	}
	// 最早的 3 人候补整组获席，获席时间为释放时刻 900
	if got := regByID(t, v, idC); got.Status != StatusPending || got.GrantedAt != 900 {
		t.Fatalf("C = %+v, want pending granted at 900", got)
	}
	// 较晚的 2 人候补不能先分到 2 席
	if got := regByID(t, v, idD); got.Status != StatusWaitlisted || got.GrantedAt != -1 {
		t.Fatalf("D = %+v, want still waitlisted", got)
	}
	if v.Remaining != 0 {
		t.Fatalf("t=900: remaining = %d, want 0", v.Remaining)
	}
}

// op 是一条外部操作脚本。
type op struct {
	at   int64
	kind string // "register" / "confirm" / "cancel"
	size int    // register 用
	id   int    // confirm/cancel 用
}

// runScript 在 [0,until] 上执行脚本；stepByStep 为 true 时每秒都调用一次 View，
// 否则只在操作时刻和终点调用。返回终点快照与每次操作（含 confirm）的错误。
func runScript(t *testing.T, capacity int, until int64, ops []op, stepByStep bool) (View, []error) {
	t.Helper()
	a := NewActivity(capacity)
	errs := make([]error, len(ops))
	nextID := 0
	opIdx := 0
	for now := int64(0); now <= until; now++ {
		for opIdx < len(ops) && ops[opIdx].at == now {
			o := ops[opIdx]
			switch o.kind {
			case "register":
				id, err := a.Register(o.at, o.size)
				if err != nil {
					t.Fatalf("t=%d: register size %d: %v", o.at, o.size, err)
				}
				nextID++
				if id != nextID {
					t.Fatalf("t=%d: register returned id %d, want %d", o.at, id, nextID)
				}
			case "confirm":
				errs[opIdx] = a.Confirm(o.at, o.id)
			case "cancel":
				errs[opIdx] = a.Cancel(o.at, o.id)
			}
			opIdx++
		}
		if stepByStep {
			checkInvariant(t, a, now)
		}
	}
	return a.View(until), errs
}

// 复杂场景脚本：跨越多轮到期与递补。
// 容量 4：
//
//	t=0    R1(3) 获席，R2(2) 候补
//	t=100  R3(1) 获席（R2 放不下被跳过）
//	t=200  R4(4) 候补
//	t=300  R5(2) 候补
//	t=900  R1 到期释放 3 席 → R2 获席@900（剩 1，R4/R5 放不下）
//	t=1000 R3 到期释放 1 席 → R5 获席@1000（R4 放不下被跳过）
//	t=1800 R2 到期释放 2 席 → 无人放得下
//	t=1900 R5 到期释放 2 席 → R4 获席@1900
//	t=2800 R4 到期取消；恰在截止时刻确认 R4 失败
func skipTestOps() []op {
	return []op{
		{at: 0, kind: "register", size: 3},   // id 1
		{at: 0, kind: "register", size: 2},   // id 2
		{at: 100, kind: "register", size: 1}, // id 3
		{at: 200, kind: "register", size: 4}, // id 4
		{at: 300, kind: "register", size: 2}, // id 5
		{at: 2800, kind: "confirm", id: 4},   // 恰在截止时刻，必须失败
	}
}

// TestSkipMatchesStepByStep 一次跳过 1 小时与逐秒查看得到相同的
// 状态、获席时间和取消时间。
func TestSkipMatchesStepByStep(t *testing.T) {
	const until = 3600
	ops := skipTestOps()

	jumpView, jumpErrs := runScript(t, 4, until, ops, false)
	stepView, stepErrs := runScript(t, 4, until, ops, true)

	if !reflect.DeepEqual(jumpView, stepView) {
		t.Fatalf("view mismatch:\n jump=%+v\n step=%+v", jumpView, stepView)
	}
	if !reflect.DeepEqual(jumpErrs, stepErrs) {
		t.Fatalf("op error mismatch: jump=%v step=%v", jumpErrs, stepErrs)
	}
}

// TestSkipTimeline 直接校验跳过场景的时间线：每轮占位只持续 900 秒，
// 获席时间是实际释放席位的时刻，截止时刻的确认不能复活报名。
func TestSkipTimeline(t *testing.T) {
	v, errs := runScript(t, 4, 3600, skipTestOps(), false)

	// 恰在截止时刻（1900+900=2800）的确认必须失败
	if errs[5] == nil {
		t.Fatal("confirm at exact deadline should fail")
	}

	want := []RegistrationView{
		{ID: 1, Size: 3, Status: StatusCancelled, GrantedAt: 0, CancelledAt: 900},
		{ID: 2, Size: 2, Status: StatusCancelled, GrantedAt: 900, CancelledAt: 1800},
		{ID: 3, Size: 1, Status: StatusCancelled, GrantedAt: 100, CancelledAt: 1000},
		{ID: 4, Size: 4, Status: StatusCancelled, GrantedAt: 1900, CancelledAt: 2800},
		{ID: 5, Size: 2, Status: StatusCancelled, GrantedAt: 1000, CancelledAt: 1900},
	}
	if !reflect.DeepEqual(v.Registrations, want) {
		t.Fatalf("registrations mismatch:\n got=%+v\nwant=%+v", v.Registrations, want)
	}
	if v.Remaining != 4 {
		t.Fatalf("remaining = %d, want 4", v.Remaining)
	}
}

// TestConfirmAtDeadline 恰在截止时刻确认不能成功，之前和之后的状态符合预期。
func TestConfirmAtDeadline(t *testing.T) {
	a := NewActivity(2)
	id, err := a.Register(100, 2) // 获席@100，截止 1000
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Confirm(999, id); err != nil {
		t.Fatalf("confirm before deadline should succeed: %v", err)
	}
	v := a.View(1000)
	if got := regByID(t, v, id); got.Status != StatusConfirmed {
		t.Fatalf("confirmed reg should not expire, got %+v", got)
	}

	// 未确认的报名在截止时刻到期，此时确认失败且不能复活
	id2, err := a.Register(1100, 1) // 候补（剩 0 席）
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Cancel(1200, id); err != nil { // 释放 2 席 → id2 获席@1200，截止 2100
		t.Fatal(err)
	}
	if err := a.Confirm(2100, id2); err == nil {
		t.Fatal("confirm at exact deadline should fail")
	}
	v = a.View(2100)
	if got := regByID(t, v, id2); got.Status != StatusCancelled || got.CancelledAt != 2100 {
		t.Fatalf("reg2 = %+v, want cancelled at 2100", got)
	}
	if err := a.Confirm(2101, id2); err == nil {
		t.Fatal("confirm after expiry should not revive registration")
	}
}

// TestBasicFlow 覆盖报名、确认、取消释放席位与候补递补的基本流程。
func TestBasicFlow(t *testing.T) {
	a := NewActivity(4)

	id1, _ := a.Register(0, 3) // 获席@0
	id2, _ := a.Register(10, 2) // 候补（剩 1 席放不下）
	id3, _ := a.Register(20, 1) // 获席@20（跳过放不下的 id2）

	v := a.View(20)
	if v.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0", v.Remaining)
	}
	if got := regByID(t, v, id2); got.Status != StatusWaitlisted {
		t.Fatalf("reg2 = %+v, want waitlisted", got)
	}
	if got := regByID(t, v, id3); got.Status != StatusPending || got.GrantedAt != 20 {
		t.Fatalf("reg3 = %+v, want pending granted at 20", got)
	}

	if err := a.Confirm(30, id1); err != nil {
		t.Fatal(err)
	}
	// 取消已确认报名，释放 3 席 → id2 整组获席@40
	if err := a.Cancel(40, id1); err != nil {
		t.Fatal(err)
	}
	v = a.View(40)
	if got := regByID(t, v, id1); got.Status != StatusCancelled || got.CancelledAt != 40 {
		t.Fatalf("reg1 = %+v, want cancelled at 40", got)
	}
	if got := regByID(t, v, id2); got.Status != StatusPending || got.GrantedAt != 40 {
		t.Fatalf("reg2 = %+v, want pending granted at 40", got)
	}

	// 取消候补：退出候补，不影响他人
	id4, _ := a.Register(50, 4) // 候补
	if err := a.Cancel(60, id4); err != nil {
		t.Fatal(err)
	}
	v = a.View(60)
	if got := regByID(t, v, id4); got.Status != StatusCancelled || got.CancelledAt != 60 {
		t.Fatalf("reg4 = %+v, want cancelled at 60", got)
	}
	if err := a.Cancel(70, id4); err == nil {
		t.Fatal("double cancel should fail")
	}
	if err := a.Confirm(70, 999); err == nil {
		t.Fatal("confirm unknown id should fail")
	}
}

// TestNewRegistrationKeepsOrder 新报名不能抢在能放下的老候补之前。
func TestNewRegistrationKeepsOrder(t *testing.T) {
	a := NewActivity(4)
	id1, _ := a.Register(0, 4) // 占满
	id2, _ := a.Register(10, 3) // 候补
	id3, _ := a.Register(20, 1) // 候补（剩 0 席）

	if err := a.Cancel(30, id1); err != nil { // 释放 4 席
		t.Fatal(err)
	}
	v := a.View(30)
	// 老候补 id2 先获席，id3 随后，均在同一次调用内完成
	if got := regByID(t, v, id2); got.Status != StatusPending || got.GrantedAt != 30 {
		t.Fatalf("reg2 = %+v, want pending granted at 30", got)
	}
	if got := regByID(t, v, id3); got.Status != StatusPending || got.GrantedAt != 30 {
		t.Fatalf("reg3 = %+v, want pending granted at 30", got)
	}
	if v.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0", v.Remaining)
	}
}

// TestInvalidInput 校验非法输入。
func TestInvalidInput(t *testing.T) {
	a := NewActivity(3)
	if _, err := a.Register(0, 0); err == nil {
		t.Fatal("size 0 should fail")
	}
	if _, err := a.Register(0, 4); err == nil {
		t.Fatal("size > capacity should fail")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("capacity 0 should panic")
		}
	}()
	NewActivity(0)
}
