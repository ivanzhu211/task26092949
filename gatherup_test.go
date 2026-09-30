package gatherup

import (
	"errors"
	"reflect"
	"testing"
)

func regByID(t *testing.T, v View, id int) RegistrationView {
	t.Helper()
	for _, r := range v.Registrations {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("registration %d not found", id)
	return RegistrationView{}
}

// 同刻到期的 2 人组和 1 人组释放共 3 席时，
// 最早的 3 人候补应整组获席，不能先把 2 席分给较晚的 2 人候补。
func TestSameMomentExpiryGrantsWholeGroup(t *testing.T) {
	a, err := NewActivity(3)
	if err != nil {
		t.Fatal(err)
	}
	id1, _ := a.Register(0, 2) // 占 2 席
	id2, _ := a.Register(0, 1) // 占 1 席
	id3, _ := a.Register(0, 3) // 候补：整组 3 人
	id4, _ := a.Register(0, 2) // 候补：较晚的 2 人组

	v := a.Status(0)
	if v.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0", v.Remaining)
	}
	if got := regByID(t, v, id3).Status; got != StatusWaitlisted {
		t.Fatalf("id3 status = %v, want waitlisted", got)
	}

	// id1 与 id2 同刻到期，共释放 3 席。
	v = a.Status(900)

	if got := regByID(t, v, id1); got.Status != StatusCancelled || got.CancelledAt != 900 {
		t.Fatalf("id1 = %+v, want cancelled@900", got)
	}
	if got := regByID(t, v, id2); got.Status != StatusCancelled || got.CancelledAt != 900 {
		t.Fatalf("id2 = %+v, want cancelled@900", got)
	}
	// 最早的 3 人候补整组获席，获席时间为释放时刻。
	if got := regByID(t, v, id3); got.Status != StatusPending || got.GrantedAt != 900 {
		t.Fatalf("id3 = %+v, want pending granted@900", got)
	}
	// 较晚的 2 人候补不得先分走 2 席。
	if got := regByID(t, v, id4); got.Status != StatusWaitlisted || got.GrantedAt != -1 {
		t.Fatalf("id4 = %+v, want still waitlisted", got)
	}
	if v.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0", v.Remaining)
	}
}

// 一次跳过 1 小时与逐秒查看得到相同的状态、获席时间和取消时间；
// 跨过的每轮占位均只持续 900 秒，截止时刻的确认不能复活报名。
func TestSkipVsStepwise(t *testing.T) {
	// 场景：容量 4，多轮到期与递补跨越 2700 秒。
	ops := func(a *Activity) {
		for _, size := range []int{2, 2, 3, 1, 2} {
			if _, err := a.Register(0, size); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.Cancel(100, 1); err != nil { // id1 主动取消
			t.Fatal(err)
		}
		if err := a.Confirm(950, 4); err != nil { // id4 确认，不再过期
			t.Fatal(err)
		}
		// id5 在 1800 获席，截止 2700；恰在截止时刻确认不能成功。
		if err := a.Confirm(2700, 5); !errors.Is(err, ErrNotPending) {
			t.Fatalf("confirm at deadline = %v, want ErrNotPending", err)
		}
	}

	// 逐秒查看。
	stepwise, _ := NewActivity(4)
	for now := int64(0); now <= 3000; now++ {
		stepwise.Status(now)
		switch now {
		case 0:
			for _, size := range []int{2, 2, 3, 1, 2} {
				if _, err := stepwise.Register(0, size); err != nil {
					t.Fatal(err)
				}
			}
		case 100:
			if err := stepwise.Cancel(100, 1); err != nil {
				t.Fatal(err)
			}
		case 950:
			if err := stepwise.Confirm(950, 4); err != nil {
				t.Fatal(err)
			}
		case 2700:
			if err := stepwise.Confirm(2700, 5); !errors.Is(err, ErrNotPending) {
				t.Fatalf("stepwise confirm at deadline = %v, want ErrNotPending", err)
			}
		}
	}
	_ = ops

	// 一次跳过：中间不查看，直接跨过 1 小时以上。
	skip, _ := NewActivity(4)
	ops(skip)

	vStep := stepwise.Status(3000)
	vSkip := skip.Status(3000)

	if !reflect.DeepEqual(vStep, vSkip) {
		t.Fatalf("views differ:\nstepwise: %+v\nskip:     %+v", vStep, vSkip)
	}

	// 期望的终态：每轮占位只持续 900 秒，获席时间为实际释放时刻。
	want := []RegistrationView{
		{ID: 1, Size: 2, Status: StatusCancelled, GrantedAt: 0, CancelledAt: 100},
		{ID: 2, Size: 2, Status: StatusCancelled, GrantedAt: 0, CancelledAt: 900},
		{ID: 3, Size: 3, Status: StatusCancelled, GrantedAt: 900, CancelledAt: 1800},
		{ID: 4, Size: 1, Status: StatusConfirmed, GrantedAt: 100, CancelledAt: -1},
		{ID: 5, Size: 2, Status: StatusCancelled, GrantedAt: 1800, CancelledAt: 2700},
	}
	if !reflect.DeepEqual(vSkip.Registrations, want) {
		t.Fatalf("registrations = %+v, want %+v", vSkip.Registrations, want)
	}
	if vSkip.Remaining != 3 { // 仅 id4（1 席）确认占用
		t.Fatalf("remaining = %d, want 3", vSkip.Remaining)
	}
}

// 基本流程：报名、确认、取消、候补递补与错误处理。
func TestBasicFlow(t *testing.T) {
	a, err := NewActivity(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewActivity(0); !errors.Is(err, ErrInvalidCapacity) {
		t.Fatalf("NewActivity(0) = %v, want ErrInvalidCapacity", err)
	}
	if _, err := a.Register(0, 0); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Register size 0 = %v, want ErrInvalidSize", err)
	}

	id1, _ := a.Register(0, 2) // 占满
	id2, _ := a.Register(10, 1)
	id3, _ := a.Register(20, 1)

	v := a.Status(30)
	if got := regByID(t, v, id2).Status; got != StatusWaitlisted {
		t.Fatalf("id2 status = %v, want waitlisted", got)
	}

	// 确认后不再自动过期。
	if err := a.Confirm(40, id1); err != nil {
		t.Fatal(err)
	}
	v = a.Status(100000)
	if got := regByID(t, v, id1).Status; got != StatusConfirmed {
		t.Fatalf("id1 status = %v, want confirmed", got)
	}

	// 取消已确认报名，释放席位后按顺序递补 id2。
	if err := a.Cancel(100000, id1); err != nil {
		t.Fatal(err)
	}
	v = a.Status(100000)
	if got := regByID(t, v, id2); got.Status != StatusPending || got.GrantedAt != 100000 {
		t.Fatalf("id2 = %+v, want pending granted@100000", got)
	}
	// 释放 2 席后 id2、id3 各 1 席都能放下，同刻获席。
	if got := regByID(t, v, id3); got.Status != StatusPending || got.GrantedAt != 100000 {
		t.Fatalf("id3 = %+v, want pending granted@100000", got)
	}

	// 取消 id3 释放其占位。
	if err := a.Cancel(100001, id3); err != nil {
		t.Fatal(err)
	}
	v = a.Status(100001)
	if got := regByID(t, v, id3); got.Status != StatusCancelled || got.CancelledAt != 100001 {
		t.Fatalf("id3 = %+v, want cancelled@100001", got)
	}
	if v.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1", v.Remaining)
	}
	if err := a.Cancel(100002, id3); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("re-cancel = %v, want ErrNotCancellable", err)
	}
	if err := a.Confirm(100003, id3); !errors.Is(err, ErrNotPending) {
		t.Fatalf("confirm cancelled = %v, want ErrNotPending", err)
	}
	if err := a.Confirm(100004, 999); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("confirm unknown = %v, want ErrUnknownID", err)
	}
	if _, err := a.Register(100000, 1); !errors.Is(err, ErrNonMonotonicTime) {
		t.Fatalf("backward time = %v, want ErrNonMonotonicTime", err)
	}
}
